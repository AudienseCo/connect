// Copyright 2024 Redpanda Data, Inc.
//
// Licensed as a Redpanda Enterprise file under the Redpanda Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
// https://github.com/redpanda-data/connect/blob/main/licenses/rcl.md

package aws

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/redpanda-data/benthos/v4/public/service"
	"github.com/redpanda-data/connect/v4/internal/impl/aws/config"
)

const (
	bedpFieldModel        = "model"
	bedpFieldUserPrompt   = "prompt"
	bedpFieldSystemPrompt = "system_prompt"
	bedpFieldMaxTokens    = "max_tokens"
	bedpFieldTemp         = "stop"
	bedpFieldStop         = "temperature"
	bedpFieldTopP         = "top_p"
)

func init() {
	err := service.RegisterProcessor("aws_bedrock_chat", newBedrockConfigSpec(), newBedrockProcessor)
	if err != nil {
		panic(err)
	}
}

func newBedrockConfigSpec() *service.ConfigSpec {
	return service.NewConfigSpec().
		Summary("Generates responses to messages in a chat conversation, using the AWS Bedrock API.").
		Description(`This processor sends prompts to your chosen large language model (LLM) and generates text from the responses, using the AWS Bedrock API.
For more information, see the https://docs.aws.amazon.com/bedrock/latest/userguide[AWS Bedrock documentation^].`).
		Categories("AI").
		Version("4.34.0").
		Fields(config.SessionFields()...).
		Field(service.NewStringField(bedpFieldModel).
			Examples("amazon.titan-text-express-v1", "anthropic.claude-3-5-sonnet-20240620-v1:0", "cohere.command-text-v14", "meta.llama3-1-70b-instruct-v1:0", "mistral.mistral-large-2402-v1:0").
			Description("The model ID to use. For a full list see the https://docs.aws.amazon.com/bedrock/latest/userguide/model-ids.html[AWS Bedrock documentation^].")).
		Field(service.NewStringField(bedpFieldUserPrompt).
			Description("The prompt you want to generate a response for. By default, the processor submits the entire payload as a string.").
			Optional()).
		Field(service.NewStringField(bedpFieldSystemPrompt).
			Optional().
			Description("The system prompt to submit to the AWS Bedrock LLM.")).
		Field(service.NewIntField(bedpFieldMaxTokens).
			Optional().
			Description("The maximum number of tokens to allow in the generated response.").
			LintRule(`root = this < 1 { ["field must be greater than or equal to 1"] }`)).
		Field(service.NewFloatField(bedpFieldTemp).
			Optional().
			Description("The likelihood of the model selecting higher-probability options while generating a response. A lower value makes the model omre likely to choose higher-probability options, while a higher value makes the model more likely to choose lower-probability options.").
			LintRule(`root = if this < 0 || this > 1 { ["field must be between 0.0-1.0"] }`)).
		Field(service.NewStringListField(bedpFieldStop).
			Optional().
			Advanced().
			Description("A list of stop sequences. A stop sequence is a sequence of characters that causes the model to stop generating the response.")).
		Field(service.NewFloatField(bedpFieldTopP).
			Optional().
			Advanced().
			Description("The percentage of most-likely candidates that the model considers for the next token. For example, if you choose a value of 0.8, the model selects from the top 80% of the probability distribution of tokens that could be next in the sequence. ").
			LintRule(`root = if this < 0 || this > 1 { ["field must be between 0.0-1.0"] }`))
}

func newBedrockProcessor(conf *service.ParsedConfig, mgr *service.Resources) (service.Processor, error) {
	aconf, err := GetSession(context.Background(), conf)
	if err != nil {
		return nil, err
	}
	client := bedrockruntime.NewFromConfig(aconf)
	model, err := conf.FieldString(bedpFieldModel)
	if err != nil {
		return nil, err
	}
	p := &bedrockProcessor{
		client: client,
		model:  model,
	}
	if conf.Contains(bedpFieldUserPrompt) {
		pf, err := conf.FieldInterpolatedString(bedpFieldUserPrompt)
		if err != nil {
			return nil, err
		}
		p.userPrompt = pf
	}
	if conf.Contains(bedpFieldSystemPrompt) {
		pf, err := conf.FieldInterpolatedString(bedpFieldSystemPrompt)
		if err != nil {
			return nil, err
		}
		p.systemPrompt = pf
	}
	if conf.Contains(bedpFieldMaxTokens) {
		v, err := conf.FieldInt(bedpFieldMaxTokens)
		if err != nil {
			return nil, err
		}
		mt := int32(v)
		p.maxTokens = &mt
	}
	if conf.Contains(bedpFieldTemp) {
		v, err := conf.FieldFloat(bedpFieldTemp)
		if err != nil {
			return nil, err
		}
		t := float32(v)
		p.temp = &t
	}
	if conf.Contains(bedpFieldStop) {
		stop, err := conf.FieldStringList(bedpFieldStop)
		if err != nil {
			return nil, err
		}
		p.stop = stop
	}
	if conf.Contains(bedpFieldTopP) {
		v, err := conf.FieldFloat(bedpFieldTopP)
		if err != nil {
			return nil, err
		}
		tp := float32(v)
		p.topP = &tp
	}
	return p, nil
}

type bedrockProcessor struct {
	client *bedrockruntime.Client
	model  string

	userPrompt   *service.InterpolatedString
	systemPrompt *service.InterpolatedString
	maxTokens    *int32
	stop         []string
	temp         *float32
	topP         *float32
}

func (b *bedrockProcessor) Process(ctx context.Context, msg *service.Message) (service.MessageBatch, error) {
	prompt, err := b.computePrompt(msg)
	if err != nil {
		return nil, err
	}
	input := &bedrockruntime.ConverseInput{
		Messages: []bedrocktypes.Message{
			{
				Role: bedrocktypes.ConversationRoleUser,
				Content: []bedrocktypes.ContentBlock{
					&bedrocktypes.ContentBlockMemberText{
						Value: prompt,
					},
				},
			},
		},
		ModelId: &b.model,
		InferenceConfig: &bedrocktypes.InferenceConfiguration{
			MaxTokens:     b.maxTokens,
			StopSequences: b.stop,
			Temperature:   b.temp,
			TopP:          b.topP,
		},
	}
	if b.systemPrompt != nil {
		prompt, err := b.systemPrompt.TryString(msg)
		if err != nil {
			return nil, fmt.Errorf("unable to interpolate `%s`: %w", bedpFieldSystemPrompt, err)
		}
		input.System = []bedrocktypes.SystemContentBlock{
			&bedrocktypes.SystemContentBlockMemberText{Value: prompt},
		}
	}
	resp, err := b.client.Converse(ctx, input)
	if err != nil {
		return nil, err
	}
	respOut, ok := resp.Output.(*bedrocktypes.ConverseOutputMemberMessage)
	if !ok {
		return nil, fmt.Errorf("unexpected output: %T", resp)
	}
	content := respOut.Value.Content
	if len(content) != 1 {
		return nil, fmt.Errorf("unexpected number of response content: %d", len(content))
	}
	out := msg.Copy()
	switch c := content[0].(type) {
	case *bedrocktypes.ContentBlockMemberText:
		out.SetStructured(c.Value)
	default:
		return nil, fmt.Errorf("unsupported response content type: %T", content[0])
	}
	return service.MessageBatch{out}, nil
}

func (b *bedrockProcessor) computePrompt(msg *service.Message) (string, error) {
	if b.userPrompt != nil {
		return b.userPrompt.TryString(msg)
	}
	buf, err := msg.AsBytes()
	if err != nil {
		return "", err
	}
	if !utf8.Valid(buf) {
		return "", errors.New("message payload contained invalid UTF8")
	}
	return string(buf), nil
}

func (b *bedrockProcessor) Close(ctx context.Context) error {
	return nil
}
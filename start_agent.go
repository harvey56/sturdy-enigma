package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"text/template"

	"github.com/charmbracelet/log"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/genai"
)

//go:embed instruction_start.md
var instruction_start string

//go:embed story_context.md
var storyContextTemplate string

type fetchUserSessionArgs struct {
	SessionID string `json:"session_id" description:"The unique ID of the current session."`
}

type getStartOfStoryResult struct {
	Status string `json:"status"`
	Report string `json:"report,omitempty"`
}

type responseSchema struct {
	Title              string `json:"title"`
	Opening            string `json:"opening"`
	AtmosphericSummary string `json:"atmospheric_summary"`
}

func formatInputsStory(userData UserSessionType) string {
	tmpl, err := template.New("story_context").Parse(storyContextTemplate)
	if err != nil {
		log.Error("Failed to parse story context template", "error", err)
		return fmt.Sprintf("Error parsing template: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, userData); err != nil {
		log.Error("Failed to execute story context template", "error", err)
		return fmt.Sprintf("Error executing template: %v", err)
	}
	return buf.String()
}

func fetchStoryStart(ctx context.Context, cfg Config) (agent.Agent, error) {
	model, err := gemini.NewModel(ctx, cfg.ModelName, &genai.ClientConfig{
		// either APIKey alone or Location and Project
		// APIKey:   cfg.GoogleAPIKey,
		Location: cfg.Location,
		Project:  cfg.ProjectId,
	})
	if err != nil {
		fmt.Printf("error creating the model : %v \n", err)
		return nil, err
	}

	var temperature float32 = 0.8
	var topP float32 = 0.95

	firestoreAgent, _ := llmagent.New(llmagent.Config{
		Name:        "firestore_fetcher",
		Model:       model,
		Description: "A creative agent that starts a story based on user data from Firestore.",
		Instruction: instruction_start,
		GenerateContentConfig: &genai.GenerateContentConfig{
			Temperature: &temperature,
			TopP:        &topP,
		},
		OutputKey: "current_story",
	})

	return firestoreAgent, nil
}

package main

import (
	"context"
	_ "embed"
	"encoding/json"

	"fmt"
	"iter"
	"log"
	"strconv"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

//go:embed instruction_update.md
var instruction_update string

//go:embed instruction_director.md
var instruction_director string

// storyHistory holds the story between executions. For a real application with
// multiple users, you would replace this with a database or a persistent cache.
var storyHistory string

type StoryState struct {
	History     []string `json:"history"`
	LastSegment string   `json:"last_segment"`
}

type StoryUpdate struct {
	StoryText     string   `json:"story_text"`
	Option1       string   `json:"option_1"`
	Option2       string   `json:"option_2"`
	ImagePrompt   string   `json:"image_prompt,omitempty"`
	ImageURLs     []string `json:"image_urls,omitempty"`
	NextStoryText string   `json:"next_story_text,omitempty"`
	NextOption1   string   `json:"next_option_1,omitempty"`
	NextOption2   string   `json:"next_option_2,omitempty"`
}

type InteractiveStoryAgent struct {
	storyGenerator agent.Agent
}

func NewStoryFlowAgent(
	storyGenerator agent.Agent,
) (agent.Agent, error) {
	// We don't need a custom struct if we are just using the SequentialAgent directly,
	// but sticking to your pattern, we can return the passed agent (which will be the sequential one).
	orchestrator := &InteractiveStoryAgent{
		storyGenerator: storyGenerator,
	}

	// agent.New creates the final agent, wiring up the Run method.
	return agent.New(agent.Config{
		Name:        "StoryFlowAgent",
		Description: "Orchestrates story generation.",
		SubAgents:   []agent.Agent{storyGenerator},
		Run:         orchestrator.Run,
	})

}

func getPastStory(ctx tool.Context, args fetchUserSessionArgs) (string, error) {
	previousContent := p.story["story"]

	// previousContent can be empty if the user disconnected - fetch from firestore
	if previousContent == "" {
		userData, err := FetchUserSession(ctx, p.fsClient, args.SessionID)
		if err != nil {
			return "", err
		}

		//! concatener les differents recits stoques dans firestore
		previousContent = formatInputsStory(*userData)
	}

	return previousContent, nil
}

func getCharacterSheet(ctx tool.Context, args fetchUserSessionArgs) (string, error) {
	userData, err := FetchUserSession(ctx, p.fsClient, args.SessionID)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(userData)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func fetchStoryUpdate(ctx context.Context, cfg Config, user_choice UserChoice, chapter int, imgCh chan<- ImageGenResult) (*StoryUpdate, error) {
	m, err := gemini.NewModel(ctx, cfg.ModelName, &genai.ClientConfig{})
	if err != nil {
		log.Fatalf("Failed to create model: %v", err)
	}

	fetchStoryTool, err := functiontool.New(
		functiontool.Config{
			Name:        "get_past_story",
			Description: "Retrieves the past story.",
		},
		getPastStory,
	)
	if err != nil {
		log.Fatal(err)
	}

	characterSheetTool, err := functiontool.New(
		functiontool.Config{
			Name:        "get_character_sheet",
			Description: "Retrieves the character's profile, including skills, competences, weapons, and tools.",
		},
		getCharacterSheet,
	)
	if err != nil {
		log.Fatal(err)
	}

	// Story Director
	directorAgent, err := llmagent.New(llmagent.Config{
		Name:        "NarrativeDirector",
		Model:       m,
		Description: "Decides the pacing and narrative direction for the chapter.",
		Instruction: instruction_director,
		Tools: []tool.Tool{
			fetchStoryTool,
			characterSheetTool,
		},
		OutputKey: "pacing_directive", // Output is stored in session state as 'pacing_directive'
	})
	if err != nil {
		log.Fatalf("Failed to create Director agent: %v", err)
	}

	// Story Writer
	storyGenerator, err := llmagent.New(llmagent.Config{
		Name:        "StoryUpdateGenerator",
		Model:       m,
		Description: "Generates the story update based on user input.",
		Instruction: instruction_update,
		Tools: []tool.Tool{
			fetchStoryTool,
			characterSheetTool,
		},
		OutputKey: "current_story",
	})
	if err != nil {
		log.Fatalf("Failed to create StoryGenerator agent: %v", err)
	}

	// Chain agents as sequential
	storyFlowAgent, err := sequentialagent.New(sequentialagent.Config{
		AgentConfig: agent.Config{
			Name: "DirectorWriterPipeline",
			SubAgents: []agent.Agent{
				directorAgent,
				storyGenerator,
			},
		},
	})
	if err != nil {
		log.Fatalf("Failed to create story flow agent: %v", err)
	}

	// Run the Agent
	sessionService := session.InMemoryService()
	initialState := map[string]any{
		"user_choice":  user_choice.Choice,
		"session_id":   user_choice.SessionID,
		"chapter":      chapter,
		"max_chapters": 20,
	}
	sessionInstance, err := sessionService.Create(ctx, &session.CreateRequest{
		AppName: cfg.AppName,
		UserID:  cfg.UserID,
		State:   initialState,
	})
	if err != nil {
		log.Fatalf("Failed to create session: %v", err)
	}

	r, err := runner.New(runner.Config{
		AppName:        cfg.AppName,
		Agent:          storyFlowAgent,
		SessionService: sessionService,
	})
	if err != nil {
		log.Fatalf("Failed to create runner: %v", err)
	}

	input := genai.NewContentFromText("Start the story.", genai.RoleUser)
	events := r.Run(ctx, cfg.UserID, sessionInstance.Session.ID(), input, agent.RunConfig{
		StreamingMode: agent.StreamingModeSSE,
	})

	var finalResponse string
	for event, err := range events {
		if err != nil {
			log.Fatalf("An error occurred during agent execution: %v", err)
		}

		finalResponse = ""

		if event.Content != nil {
			for _, part := range event.Content.Parts {
				// Accumulate text from all parts of the final response
				finalResponse += part.Text
			}
		}
	}

	fmt.Println("Agent Final Response: \n" + finalResponse)

	_, err = sessionService.Get(ctx, &session.GetRequest{
		UserID:    cfg.UserID,
		AppName:   cfg.AppName,
		SessionID: sessionInstance.Session.ID(),
	})

	if err != nil {
		log.Fatalf("Failed to retrieve final session: %v", err)
	}

	jsonOutput, err := ProcessHandler(finalResponse)
	if err != nil {
		fmt.Println("Error:", err)
		fmt.Printf("jsonOutput : %s \n", jsonOutput)

		// Fallback: Fetch previous chapter from Firestore so the UI has content to display (retry)
		var fallback StoryUpdate
		if chapter > 1 && p.fsClient != nil {
			prevID := strconv.Itoa(chapter - 1)
			// We try to retrieve the previous chapter to repopulate the UI options
			if title, ok := p.story["title"]; ok {
				doc, errSnap := p.fsClient.Collection("sessions").Doc(p.FirestoreSessionID).Collection(title).Doc(prevID).Get(ctx)
				if errSnap == nil {
					data := doc.Data()
					if val, ok := data["story_text"].(string); ok {
						fallback.StoryText = val
					}
					if val, ok := data["option_1"].(string); ok {
						fallback.Option1 = val
					}
					if val, ok := data["option_2"].(string); ok {
						fallback.Option2 = val
					}
					if val, ok := data["image_url"].(string); ok {
						fallback.ImageURLs = []string{val}
					}
				}
			}
		}

		return &fallback, err
	}

	var update StoryUpdate

	if err := json.Unmarshal([]byte(jsonOutput), &update); err != nil {
		// If the response isn't the expected JSON, treat it as raw text
		log.Printf("Could not unmarshal response into StoryUpdate JSON, treating as raw text: %v", err)
		update.StoryText = finalResponse
		// storyHistory += finalResponse
	}
	// else {
	// 	// If JSON unmarshaling succeeds, append only the story_text part.
	// 	if storyHistory != "" {
	// 		storyHistory += "\n\n"
	// 	}
	// 	storyHistory += p.update.StoryText
	// }

	// After parsing the story, check if an image prompt was included.
	if update.ImagePrompt != "" && imgCh != nil {
		// use context.Background() because the request context 'ctx' will likely expire
		// before the long-running image generation is complete
		go generateImage(context.Background(), cfg, update.ImagePrompt, imgCh)
	}

	// Update overall story
	// p.story["story"] = p.story["story"] + p.update.StoryText

	// return finalSession.Session.State()
	return &update, nil
}

func (s *InteractiveStoryAgent) Run(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return s.storyGenerator.Run(ctx)
}

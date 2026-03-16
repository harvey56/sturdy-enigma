package main

import (
	"context"
	_ "embed"
	"regexp"
	"strconv"
	"strings"

	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/patrickmn/go-cache"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// Global styling for logging allows to flag long running processes
var (
	timeWarn            = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFF00"))
	timeUrgentWarn      = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF0000"))
	thresholdWarn       = time.Second * 10
	thresholdUrgentWarn = time.Second * 30
)

type Config struct {
	Port                   string
	ModelName              string
	ImageModelName         string
	FallbackImageModelName string
	AppName                string
	UserID                 string
	GoogleAPIKey           string
	ProjectId              string
	Location               string
	TLSCert                string
	TLSKey                 string
	FirebaseStorageBucket  string
	FirebaseServiceAccount string
}

type InitiateStoryRequest struct {
	SessionID string `json:"session_id"`
	UserSessionType
}

type ImageGenResult struct {
	URL string
	Err error
}

type StartStoryRequest struct {
	start bool
}

type Program struct {
	FirestoreSessionID string
	fsClient           *firestore.Client
	story              map[string]string
	cache              *cache.Cache
	chapter            int
	imgCh              chan ImageGenResult
	update             *StoryUpdate
	pendingImages      []string
	pendingImgMutex    sync.Mutex
}

type CachedStory struct {
	Update    *StoryUpdate
	Err       error
	Done      chan struct{}
	ImageDone chan string
}

type StoryFlowAgent struct {
	firestore_agent    agent.Agent
	story_update_agent agent.Agent
	revisionLoopAgent  agent.Agent
	postProcessorAgent agent.Agent
}

type UserChoice struct {
	SessionID string `json:"session_id"`
	Choice    string `json:"choice"`
}
type responseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func LoadConfig() Config {

	if err := godotenv.Load(); err != nil {
		log.Debug("No .env file found in current directory")
	}

	cfg := Config{
		Port:                   os.Getenv("PORT"),
		ModelName:              os.Getenv("GEMINI_MODEL_NAME"),
		ImageModelName:         os.Getenv("GEMINI_IMAGE_MODEL_NAME"),
		FallbackImageModelName: os.Getenv("GEMINI_FALLBACK_IMAGE_MODEL_NAME"),
		AppName:                "ai-story-app",
		UserID:                 os.Getenv("USER_ID"),
		GoogleAPIKey:           os.Getenv("GOOGLE_API_KEY"),
		ProjectId:              os.Getenv("PROJECT_ID"),
		Location:               os.Getenv("LOCATION"),
		TLSCert:                os.Getenv("TLS_CERT"),
		TLSKey:                 os.Getenv("TLS_KEY"),
		FirebaseStorageBucket:  os.Getenv("FIREBASE_STORAGE_BUCKET"),
		FirebaseServiceAccount: os.Getenv("FIREBASE_SERVICE_ACCOUNT"),
	}

	if cfg.Port == "" {
		cfg.Port = "8080" // Default fallback
	}

	if cfg.ModelName == "" {
		cfg.ModelName = "gemini-2.5-pro"
	}

	return cfg
}

var p = &Program{}

func main() {
	// Configure charmbracelet/log
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)
	log.SetPrefix("ai-story-agent")

	cfg := LoadConfig()
	log.Info("Starting ai-story-agent", "config", cfg.ModelName, "port", cfg.Port)

	ctx := context.Background()

	// Initialize Firestore Client
	fsClient, err := firestore.NewClient(ctx, cfg.ProjectId)
	if err != nil {
		log.Fatalf("Failed to create firestore client: %v", err)
	}
	defer fsClient.Close()

	p.fsClient = fsClient

	// Initialise the story map
	p.story = make(map[string]string)

	// Initialise the cache
	// Default expiration: 20 mins, Cleanup interval: 30 mins
	p.cache = cache.New(20*time.Minute, 30*time.Minute)

	// Initialiase the image channel
	p.imgCh = make(chan ImageGenResult)

	p.pendingImages = []string{}

	// Initialise content of update
	p.update = &StoryUpdate{
		StoryText:     "",
		Option1:       "",
		Option2:       "",
		NextStoryText: "",
		NextOption1:   "",
		NextOption2:   "",
		ImagePrompt:   "",
		ImageURLs:     []string{},
	}

	// Story starts at chapter 1
	p.chapter = 1

	// Setup Router
	mux := http.NewServeMux()

	// Endpoint to receive character and companion profiles and launch the story intro - STREAMING
	mux.HandleFunc("POST /api/initiate_story", func(w http.ResponseWriter, r *http.Request) {
		// Set headers for SSE
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		var req InitiateStoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Load FireStore Session ID for use in the main
		p.HandleLoadFireStoreSesionID(req.SessionID)
		fmt.Printf("Firestore session ID : %s \n", req.SessionID)

		// Fetch user data manually to inject into prompt, avoiding tool round-trip latency
		userData, err := FetchUserSession(r.Context(), p.fsClient, req.SessionID)
		if err != nil {
			log.Error("Failed to fetch user session", "error", err)
			http.Error(w, "Failed to fetch user session", http.StatusInternalServerError)
			return
		}
		storyContext := formatInputsStory(*userData)

		// Call the agent that allows starting the story
		firestore_agent, err := fetchStoryStart(ctx, cfg)

		sessionService := session.InMemoryService()

		firestoreSessionID := p.FirestoreSessionID
		initialState := map[string]any{
			"firestore_session_id": firestoreSessionID,
		}

		sessionInstance, err := sessionService.Create(ctx, &session.CreateRequest{
			AppName: cfg.AppName,
			UserID:  cfg.UserID,
			State:   initialState,
		})
		if err != nil {
			log.Fatalf("Failed to create session: %v", err)
		}

		ru, err := runner.New(runner.Config{
			AppName:        cfg.AppName,
			Agent:          firestore_agent,
			SessionService: sessionService,
		})
		if err != nil {
			log.Fatalf("Failed to create runner: %v", err)
		}

		input := genai.NewContentFromText(fmt.Sprintf("STORY CONTEXT:\n%s\n\nTASK: Please start the story for session %s based on the context above.", storyContext, firestoreSessionID), genai.RoleUser)

		// Run the Agent
		events := ru.Run(ctx, cfg.UserID, sessionInstance.Session.ID(), input, agent.RunConfig{
			StreamingMode: agent.StreamingModeSSE,
		})

		var finalResponse string
		for event, err := range events {
			if err != nil {
				log.Errorf("An error occurred during agent execution: %v", err)
			}

			for _, part := range event.Content.Parts {
				finalResponse += part.Text
				if part.Text != "" {
					fmt.Fprint(w, part.Text)
				}

				flusher.Flush() // This sends the data to Flutter immediately
				time.Sleep(500 * time.Millisecond)
			}
		}

		_, err = sessionService.Get(ctx, &session.GetRequest{
			UserID:    cfg.UserID,
			AppName:   cfg.AppName,
			SessionID: sessionInstance.Session.ID(),
		})

		if err != nil {
			log.Fatalf("Failed to retrieve final session: %v", err)
		}

		p.story = map[string]string{
			"title":   uuid.New().String(),
			"opening": finalResponse,
		}

		// store the intro in Firestore
		_, err = p.fsClient.Collection("sessions").Doc(p.FirestoreSessionID).Collection(p.story["title"]).Doc("intro").Set(ctx, map[string]interface{}{
			"opening": finalResponse,
		})
		if err != nil {
			log.Printf("An error has occurred: %s", err)
		}

	})

	// Endpoint to receive instruction to start the interactive story
	mux.HandleFunc("POST /api/start_story", func(w http.ResponseWriter, r *http.Request) {
		var user_choice UserChoice
		if err := json.NewDecoder(r.Body).Decode(&user_choice); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		var start *StoryUpdate
		var err error

		// Capture current state for background operations
		currentChapter := p.chapter
		currentTitle := p.story["title"]

		// Check cache
		// Check if we have pre-generated this branch
		cacheKey := fmt.Sprintf("%s:%s", user_choice.SessionID, user_choice.Choice)
		cachedItem, found := p.cache.Get(cacheKey)

		startfunc := time.Now()

		if found {
			cs := cachedItem.(*CachedStory)
			// Wait for the prefetch to finish if it's still running
			<-cs.Done
			if cs.Err == nil {
				log.Info("Cache hit for choice", "choice", user_choice.Choice)
				start = cs.Update

				// If the cached version doesn't have an image yet, but we have a channel to wait for it
				if len(start.ImageURLs) == 0 && cs.ImageDone != nil {
					log.Info("Cache hit but waiting for image in background...")
					go func() {
						if url := <-cs.ImageDone; url != "" {
							p.updateStoryImageInDB(context.Background(), p.FirestoreSessionID, currentTitle, currentChapter, url)
							p.addToPendingImages(url)
						}
					}()
				}
			} else {
				// If prefetch failed, treat as miss
				found = false
			}
		}

		if !found {
			log.Info("Cache MISS for choice, generating now", "choice", user_choice.Choice)
			// For a cache miss, generate on-demand. create a local channel
			// to wait for the image generation result for this specific request
			localImgCh := make(chan ImageGenResult, 1)
			start, err = fetchStoryUpdate(r.Context(), cfg, user_choice, p.chapter, localImgCh)
			if err != nil {
				http.Error(w, "Failed to generate story", http.StatusInternalServerError)
				return
			}

			// For cache miss, do NOT wait for the image
			go func() {
				res := <-localImgCh
				if res.Err != nil {
					log.Error("Background image gen failed", "err", res.Err)
				} else {
					// Image generated but too late for this response
					log.Info("Background image gen success, updating DB", "url", res.URL)
					p.updateStoryImageInDB(context.Background(), p.FirestoreSessionID, currentTitle, currentChapter, res.URL)
					p.addToPendingImages(res.URL)
				}
			}()
		}

		// Flush any pending images (from previous chapters or late arrivals) into this response
		pending := p.popPendingImages()
		start.ImageURLs = append(start.ImageURLs, pending...)

		// Update the global story state (commit the chosen path)
		p.story["story"] = p.story["story"] + start.StoryText

		// Store the latest chapter in the DB
		err = storeStoryInDB(r.Context(), p.FirestoreSessionID, p.story["title"], p.chapter, *start)
		if err != nil {
			log.Printf("An error has occurred: %s", err)
		}

		// Trigger pre-fetching for the NEXT options
		go p.prefetchBranches(context.Background(), cfg, user_choice.SessionID, p.chapter+1, start.Option1, start.Option2)

		elapsed := time.Since(startfunc)
		fmt.Printf("start function took %s\n", elapsed)

		p.chapter += 1

		log.Info("Saving profile and starting story", "session_id", p.FirestoreSessionID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(start)
	})

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("OK")); err != nil {
			log.Printf("Failed to write response: %v", err)
		}
	})

	// Setup Server with Middleware
	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: recoveryMiddleware(loggingMiddleware(mux)),
	}

	// Start Server (Graceful Shutdown)
	go func() {
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			log.Info("Serving HTTPS", "cert", cfg.TLSCert, "key", cfg.TLSKey)
			if err := srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("Server failed: %v", err)
			}
		} else {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("Server failed: %v", err)
			}
		}
	}()
	log.Info("Server started", "addr", srv.Addr)

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("Shutting down server...")

	// Create a deadline to wait for
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Info("Server exited")
}

// prefetchBranches starts background generation for the next possible story segments
func (p *Program) prefetchBranches(ctx context.Context, cfg Config, sessionID string, nextChapter int, opt1, opt2 string) {
	prefetch := func(choice string) {
		if choice == "" {
			return
		}

		cacheKey := fmt.Sprintf("%s:%s", sessionID, choice)
		if _, found := p.cache.Get(cacheKey); found {
			return // Already generated
		}

		// Create a pending cache entry immediately
		cs := &CachedStory{Done: make(chan struct{}), ImageDone: make(chan string, 1)}
		p.cache.Set(cacheKey, cs, cache.DefaultExpiration)

		log.Info("Pre-fetching branch", "choice", choice, "chapter", nextChapter)

		// Dedicated channel for this pre-fetch to capture the image URL when it's ready
		localImgCh := make(chan ImageGenResult)

		userChoice := UserChoice{SessionID: sessionID, Choice: choice}
		update, err := fetchStoryUpdate(ctx, cfg, userChoice, nextChapter, localImgCh)

		cs.Update = update
		cs.Err = err
		close(cs.Done) // Signal completion to any waiters

		if err != nil {
			log.Error("Pre-fetch failed", "choice", choice, "error", err)
			// We leave the failed entry in cache (or could delete it)
			// so waiters see the error and fall back to on-demand.
			return
		}

		// Wait for image in background
		go func() {
			res := <-localImgCh
			if res.Err == nil {
				// Notify any active listeners (like start_story) that the image is ready
				cs.ImageDone <- res.URL
				close(cs.ImageDone)

				// Create a copy to update the cache safely without race conditions on the pointer
				newUpdate := *update
				newUpdate.ImageURLs = []string{res.URL}

				// Shift fields for the UI to handle the image display correctly
				newUpdate.NextStoryText = newUpdate.StoryText
				newUpdate.NextOption1 = newUpdate.Option1
				newUpdate.NextOption2 = newUpdate.Option2
				newUpdate.StoryText = ""
				newUpdate.Option1 = ""
				newUpdate.Option2 = ""

				// Update the cache with the new version containing the image
				newCS := &CachedStory{
					Update: &newUpdate,
					Done:   make(chan struct{}), // Already done (text)
				}
				close(newCS.Done) // Signal done
				p.cache.Set(cacheKey, newCS, cache.DefaultExpiration)
				log.Info("Pre-fetched item updated in cache with image URL and shifted fields.")
			}
		}()
	}

	go prefetch(opt1)
	go prefetch(opt2)
}

func (p *Program) HandleLoadFireStoreSesionID(session_id string) {
	p.FirestoreSessionID = session_id
}

func (rw *responseWriter) WriteHeader(code int) {
	if rw.wroteHeader {
		return
	}
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
	rw.wroteHeader = true
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Error("Panic recovered", "error", err, "path", r.URL.Path)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(ww, r)

		timesince := time.Since(start)
		str := timesince.String()

		switch {
		case timesince > thresholdUrgentWarn:
			str = timeUrgentWarn.Render(str)
		case timesince > thresholdWarn:
			str = timeWarn.Render(str)
		}

		log.Info(fmt.Sprintf("%s %s %s %d %s", r.Method, r.URL.Path, r.RemoteAddr, ww.statusCode, str))
	})
}

func initiateStory(ctx context.Context, cfg Config, FirestoreSessionsID string) string {
	// Manually fetch context
	userData, err := FetchUserSession(ctx, p.fsClient, FirestoreSessionsID)
	if err != nil {
		log.Error("Failed to fetch user session", "error", err)
		return ""
	}
	storyContext := formatInputsStory(*userData)

	// Call the agent that allows starting the story
	firestore_agent, err := fetchStoryStart(ctx, cfg)

	sessionService := session.InMemoryService()

	firestoreSessionID := FirestoreSessionsID
	initialState := map[string]any{
		"firestore_session_id": firestoreSessionID,
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
		Agent:          firestore_agent,
		SessionService: sessionService,
	})
	if err != nil {
		log.Fatalf("Failed to create runner: %v", err)
	}

	input := genai.NewContentFromText(fmt.Sprintf("STORY CONTEXT:\n%s\n\nTASK: Please start the story for session %s.", storyContext, firestoreSessionID), genai.RoleUser)

	// Run the Agent
	events := r.Run(ctx, cfg.UserID, sessionInstance.Session.ID(), input, agent.RunConfig{
		StreamingMode: agent.StreamingModeSSE,
	})

	var finalResponse string
	for event, err := range events {
		if err != nil {
			log.Fatalf("An error occurred during agent execution: %v", err)
		}

		// Replace with the latest content (cumulative, so always the most complete)
		finalResponse = "" // Reset to avoid manual delta logic

		for _, part := range event.Content.Parts {
			// Accumulate text from all parts of the final response.
			finalResponse += part.Text
		}
	}

	//! save finalResponse to Firestore
	// fmt.Println("Agent Final Response (intro): " + finalResponse)

	finalSession, err := sessionService.Get(ctx, &session.GetRequest{
		UserID:    cfg.UserID,
		AppName:   cfg.AppName,
		SessionID: sessionInstance.Session.ID(),
	})

	if err != nil {
		log.Fatalf("Failed to retrieve final session: %v", err)
	}

	fmt.Println("Final Session State:", finalSession.Session.State())

	jsonOutput, err := ProcessHandler(finalResponse)
	if err != nil {
		fmt.Println("Error:", err)
		_ = responseSchema{Title: "", Opening: "", AtmosphericSummary: ""}
		return ""
	}
	var intro responseSchema
	if err := json.Unmarshal(jsonOutput, &intro); err != nil {
		fmt.Println("failed to unmarshal JSON: %w", err)
	}

	p.story = map[string]string{
		"title":   intro.Title,
		"opening": intro.Opening,
		"story":   intro.Opening,
	}

	// store the intro in Firestore
	_, err = p.fsClient.Collection("sessions").Doc(p.FirestoreSessionID).Collection(strconv.Itoa(p.chapter)).Doc("intro").Set(ctx, map[string]interface{}{
		"title":               intro.Title,
		"opening":             intro.Opening,
		"atmospheric_summary": intro.AtmosphericSummary})
	if err != nil {
		// Handle any errors in an appropriate way, such as returning them.
		log.Printf("An error has occurred: %s", err)
	}

	return string(jsonOutput)
}

func ExtractJSON(raw string) ([]byte, error) {
	// Trim any leading/trailing whitespace
	raw = strings.TrimSpace(raw)

	var jsonStr string

	if strings.HasPrefix(raw, "```json") {
		// Regex to match the fenced code block: ```json\n ... \n```
		// (?s) allows . to match newlines
		re := regexp.MustCompile("(?s)^```json\\s*\\n(.*)\\n```$")

		matches := re.FindStringSubmatch(raw)
		if len(matches) != 2 {
			return nil, fmt.Errorf("no JSON code block found")
		}

		jsonStr = strings.TrimSpace(matches[1])
	} else {
		// Attempt to extract JSON from text if explicit code blocks aren't found
		// This handles cases where the model outputs narrative text before the JSON (it happens sometimes)
		start := strings.Index(raw, "{")
		end := strings.LastIndex(raw, "}")
		if start != -1 && end != -1 && start < end {
			jsonStr = raw[start : end+1]
		} else {
			jsonStr = raw
		}
	}

	// Validate by unmarshaling
	var data interface{}
	// fmt.Println(jsonStr)
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		log.Printf("invalid jsonStr : %v \n", jsonStr)
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	// Remarshal cleanly (without indent for compact format, or use MarshalIndent if needed)
	return json.Marshal(data)
}

func ProcessHandler(rawInput string) ([]byte, error) {
	jsonBytes, err := ExtractJSON(rawInput)
	if err != nil {
		return nil, err
	}
	return jsonBytes, nil
}

func (p *Program) addToPendingImages(url string) {
	p.pendingImgMutex.Lock()
	defer p.pendingImgMutex.Unlock()
	p.pendingImages = append(p.pendingImages, url)
	log.Info("Added image to pending queue", "url", url, "queue_size", len(p.pendingImages))
}

func (p *Program) popPendingImages() []string {
	p.pendingImgMutex.Lock()
	defer p.pendingImgMutex.Unlock()
	images := p.pendingImages
	p.pendingImages = []string{}
	return images
}

func storeStoryInDB(ctx context.Context, sessionDocID string, storyCollectionID string, storyDocID int, storyUpdate StoryUpdate) (err error) {
	data := map[string]interface{}{
		"story_text": storyUpdate.StoryText,
		"option_1":   storyUpdate.Option1,
		"option_2":   storyUpdate.Option2,
	}
	if len(storyUpdate.ImageURLs) > 0 {
		data["image_url"] = storyUpdate.ImageURLs[0] // Store primary image in simple field for backward compat if needed
		data["image_urls"] = storyUpdate.ImageURLs   // Store all
	}

	// store the intro in a document
	_, err = p.fsClient.Collection("sessions").Doc(sessionDocID).Collection(storyCollectionID).Doc(strconv.Itoa(storyDocID)).Set(ctx, data)
	if err != nil {
		// Handle any errors in an appropriate way, such as returning them.
		log.Printf("An error has occurred: %s", err)
	}

	return err
}

func (p *Program) updateStoryImageInDB(ctx context.Context, sessionDocID string, storyCollectionID string, storyDocID int, imageURL string) error {
	// Only update the image_url field
	_, err := p.fsClient.Collection("sessions").
		Doc(sessionDocID).
		Collection(storyCollectionID).
		Doc(strconv.Itoa(storyDocID)).
		Update(ctx, []firestore.Update{
			{Path: "image_url", Value: imageURL},
		})

	if err != nil {
		log.Error("Failed to update story image in DB", "error", err)
	}
	return err
}

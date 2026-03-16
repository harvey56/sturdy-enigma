package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/genai"
)

// generateImage connects to the Gemini API to generate an image,
// uploads it to Firebase Storage, and returns the public URL
func generateImage(ctx context.Context, cfg Config, prompt string, resCh chan<- ImageGenResult) {
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(parentCtx, 90*time.Second)
	defer cancel()

	// Initialize Gemini Client
	genaiClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey: cfg.GoogleAPIKey,
	})
	if err != nil {
		resCh <- ImageGenResult{Err: fmt.Errorf("failed to create genai client: %w", err)}
		return
	}

	// Initialize Firebase Storage Client
	storageClient, err := storage.NewClient(ctx)
	if err != nil {
		resCh <- ImageGenResult{Err: fmt.Errorf("failed to create storage client: %w", err)}
		return
	}
	defer storageClient.Close()

	bucket := storageClient.Bucket(cfg.FirebaseStorageBucket)

	log.Println("Generating image with prompt:", prompt)

	userData, err := FetchUserSession(ctx, p.fsClient, p.FirestoreSessionID)
	if err != nil {
		resCh <- ImageGenResult{Err: fmt.Errorf("failed to fetch user session for image generation: %w", err)}
		return
	}

	// Fetch the main character image URL from Firestore
	characterImgURL, err := FetchCharacterImage(ctx, p.fsClient, p.FirestoreSessionID, userData)
	if err != nil {
		resCh <- ImageGenResult{Err: fmt.Errorf("failed to fetch character image: %w", err)}
		return
	}

	// Fetch the companion image from Firestore
	companionImgURL, err := FetchCompanionImage(ctx, p.fsClient, p.FirestoreSessionID, userData)
	if err != nil {
		resCh <- ImageGenResult{Err: fmt.Errorf("failed to fetch companion image: %w", err)}
		return
	}

	characterImageData := getImgDataFromImgURL(ctx, cfg, bucket, characterImgURL, resCh)
	companionImageData := getImgDataFromImgURL(ctx, cfg, bucket, companionImgURL, resCh)

	// Call Image Generation Model
	parts := []*genai.Part{
		{
			InlineData: &genai.Blob{Data: characterImageData, MIMEType: "image/png"},
		},
		{
			InlineData: &genai.Blob{Data: companionImageData, MIMEType: "image/png"},
		},
		{
			Text: "Use the below prompt to generate the image. " +
				"If the image mentions about the character, use the character from the provided image \n" +
				"If the image mentions about the companion, use also the companion from the provided image \n" +
				prompt,
		},
	}

	contents := []*genai.Content{
		genai.NewContentFromParts(parts, genai.RoleUser),
	}

	genConfig := &genai.GenerateContentConfig{
		Temperature: Ptr[float32](0.7),
		TopP:        Ptr[float32](0.5),
		TopK:        Ptr[float32](2.0),
		ImageConfig: &genai.ImageConfig{
			AspectRatio: "9:16",
			ImageSize:   "1K",
			// OutputMIMEType: "image/jpeg", // set to JPEG to speed things up, but not yet supported by Gemini API
			// OutputCompressionQuality: Ptr[int32](75), // not yet supported by Gemini API
		},
		ResponseModalities: []string{"IMAGE"},
	}

	resp, err := genaiClient.Models.GenerateContent(ctx,
		cfg.ImageModelName,
		contents,
		genConfig)
	if err != nil {
		// Check for the specific high-demand error to attempt a fallback
		if strings.Contains(err.Error(), "This model is currently experiencing high demand") {
			log.Printf("Primary image model '%s' is busy. Trying fallback model '%s'.", cfg.ImageModelName, cfg.FallbackImageModelName)

			// Ensure a fallback model is configured before trying to use it.
			if cfg.FallbackImageModelName == "" {
				resCh <- ImageGenResult{Err: fmt.Errorf("primary image model is busy, and no fallback model is configured")}
				return
			}

			// Cancel the original timed context and create a new one to reset the timer
			cancel()
			ctx, cancel = context.WithTimeout(parentCtx, 120*time.Second)
			defer cancel()

			// Retry with the fallback model
			resp, err = genaiClient.Models.GenerateContent(ctx,
				cfg.FallbackImageModelName,
				contents,
				genConfig)
			if err != nil {
				// If the fallback also fails, return error
				resCh <- ImageGenResult{Err: fmt.Errorf("failed to generate content with fallback model: %w", err)}
				return
			}
		} else {
			// For any other error, just fail immediately
			log.Printf("Error generating the image with model '%s': %v", cfg.ImageModelName, err)
			resCh <- ImageGenResult{Err: fmt.Errorf("failed to generate content: %w", err)}
			return
		}
	}

	// Extract Image Data from Response
	var imageData []byte
	if resp != nil && len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
		for _, part := range resp.Candidates[0].Content.Parts {
			if part.InlineData != nil {
				imageData = part.InlineData.Data
				break
			}
		}
	}

	if len(imageData) == 0 {
		debugPrint(resp) // Print the response for debugging if no image is found
		resCh <- ImageGenResult{Err: errors.New("no image data found in the model's response")}
		return
	}
	log.Println("Image generated successfully, size:", len(imageData), "bytes")

	// Creation of the image and add to Firebase storage
	imageUUID := uuid.New().String()
	objectName := fmt.Sprintf("story_images/%s.png", imageUUID)
	wc := bucket.Object(objectName).NewWriter(ctx)
	wc.ContentType = "image/png"

	// Upload Image to Storage
	log.Println("Uploading image to Firebase Storage at:", objectName)
	if _, err := io.Copy(wc, bytes.NewReader(imageData)); err != nil {
		resCh <- ImageGenResult{Err: fmt.Errorf("io.Copy: %w", err)}
		return
	}
	if err := wc.Close(); err != nil {
		resCh <- ImageGenResult{Err: fmt.Errorf("Writer.Close: %w", err)}
		return
	}

	resCh <- ImageGenResult{URL: fmt.Sprintf("gs://%s/%s", cfg.FirebaseStorageBucket, objectName)}
}

func FetchCharacterImage(ctx context.Context, client *firestore.Client, sessionID string, userData *UserSessionType) (*string, error) {
	collectionRef := client.Collection(userData.Theme).Doc("character").Collection(strings.ToLower(userData.CharacterProfile.Race))
	query := collectionRef.Where("bodyType", "==", userData.CharacterProfile.BodyType).
		Where("gender", "==", userData.CharacterProfile.Gender).
		Where("clothes", "==", userData.CharacterProfile.Clothes).
		Limit(1)

	iter := query.Documents(ctx)
	defer iter.Stop()
	doc, err := iter.Next()
	if err == iterator.Done {
		return nil, fmt.Errorf("no character image found for criteria: bodyType=%s, gender=%s, clothes=%s",
			userData.CharacterProfile.BodyType, userData.CharacterProfile.Gender, userData.CharacterProfile.Clothes)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query character image: %w", err)
	}

	imageUrlInterface, ok := doc.Data()["image_url_character"]
	if !ok {
		return nil, fmt.Errorf("field 'image_url_character' not found in character document")
	}

	characterURL, ok := imageUrlInterface.(string)
	if !ok {
		return nil, fmt.Errorf("field 'image_url_character' is not a string, but %T", imageUrlInterface)
	}

	return &characterURL, nil
}

func FetchCompanionImage(ctx context.Context, client *firestore.Client, sessionID string, userData *UserSessionType) (*string, error) {
	collectionRef := client.Collection(userData.Theme).Doc("companion").Collection("animal")
	query := collectionRef.Where("race", "==", strings.ToLower(userData.CompanionProfile.Race)).Limit(1)

	iter := query.Documents(ctx)
	defer iter.Stop()
	doc, err := iter.Next()
	if err == iterator.Done {
		return nil, fmt.Errorf("no companion image found for race: %s", userData.CompanionProfile.Race)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query companion image: %w", err)
	}

	imageUrlInterface, ok := doc.Data()["image_url"]
	if !ok {
		return nil, fmt.Errorf("field 'image_url_companion' not found in companion document")
	}

	companionURL, ok := imageUrlInterface.(string)
	if !ok {
		return nil, fmt.Errorf("field 'image_url_companion' is not a string, but %T", imageUrlInterface)
	}

	return &companionURL, nil
}

func GetSignedURL(cfg Config, bucket *storage.BucketHandle, objectName string) (string, error) {
	opts := &storage.SignedURLOptions{
		GoogleAccessID: cfg.FirebaseServiceAccount,
		Scheme:         storage.SigningSchemeV4,
		Method:         "GET",
		Expires:        time.Now().Add(15 * time.Minute),
	}

	url, err := bucket.SignedURL(objectName, opts)
	if err != nil {
		return "", err
	}
	return url, nil
}

func getObjectName(gsURL string) string {
	u, err := url.Parse(gsURL)
	if err != nil || u.Scheme != "gs" {
		return ""
	}
	// u.Path will be like "/fantasy/953A87A1-8A1E-363A.png"
	// trim the leading slash to get the object name
	return strings.TrimPrefix(u.Path, "/")
}

func getImgDataFromImgURL(ctx context.Context, cfg Config, bucket *storage.BucketHandle, imgURL *string, resCh chan<- ImageGenResult) []byte {
	// The URL from firestore is a gs:// URL, I need to get the object name from it
	objectNameForCharacter := getObjectName(*imgURL)
	if objectNameForCharacter == "" {
		resCh <- ImageGenResult{Err: fmt.Errorf("invalid character image gs:// URL from firestore: %s", *imgURL)}
		return nil
	}

	// download the character image to send its bytes to the Gemini API
	characterObject := bucket.Object(objectNameForCharacter)
	rc, err := characterObject.NewReader(ctx)
	if err != nil {
		resCh <- ImageGenResult{Err: fmt.Errorf("failed to create reader for character image object: %w", err)}
		return nil
	}
	defer rc.Close()

	characterImageData, err := io.ReadAll(rc)
	if err != nil {
		resCh <- ImageGenResult{Err: fmt.Errorf("failed to read character image data: %w", err)}
		return nil
	}
	return characterImageData
}

func debugPrint[T any](r *T) {

	response, err := json.MarshalIndent(*r, "", "  ")
	if err != nil {
		log.Printf("Failed to marshal debug print object: %v", err)
		return
	}

	fmt.Println(string(response))
}

// Ptr is a helper function to get a pointer to a value
func Ptr[T any](v T) *T {
	return &v
}

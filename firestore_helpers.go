package main

import (
	"context"

	"cloud.google.com/go/firestore"
)

type UserSessionType struct {
	CharacterProfile CharacterProfile `firestore:"characterProfile" json:"characterProfile"`
	CompanionProfile CompanionProfile `firestore:"companionProfile" json:"companionProfile"`
	Competences      Competences      `firestore:"competences" json:"competences"`
	Skills           Skills           `firestore:"skills" json:"skills"`
	Equipment        []string         `firestore:"equipment" json:"equipment"`
	Weapons          []string         `firestore:"weapons" json:"weapons"`
	Theme            string           `firestore:"theme" json:"theme"`
	StoryState
}

type CharacterProfile struct {
	Race     string `firestore:"race" json:"race"`
	BodyType string `firestore:"bodyType" json:"bodyType"`
	Clothes  string `firestore:"clothes" json:"clothes"`
	Gender   string `firestore:"gender" json:"gender"`
	Name     string `firestore:"name" json:"name"`
	Feature  string `firestore:"feature" json:"feature"`
}

type CompanionProfile struct {
	Gender string `firestore:"gender" json:"gender"`
	Race   string `firestore:"race" json:"race"`
}

type Competences struct {
	AnimalEmpathy       int8 `firestore:"animalEmpathy" json:"animalEmpathy"`
	CelestialNavigation int8 `firestore:"celestialNavigation" json:"celestialNavigation"`
	CriticalStrike      int8 `firestore:"criticalStrike" json:"criticalStrike"`
	Engineering         int8 `firestore:"engineering" json:"engineering"`
	HerbalismAndAlchemy int8 `firestore:"herbalismAndAlchemy" json:"herbalismAndAlchemy"`
	Intimidation        int8 `firestore:"intimidation" json:"intimidation"`
	Linguistics         int8 `firestore:"linguistics" json:"linguistics"`
	ShadowBlending      int8 `firestore:"shadowBlending" json:"shadowBlending"`
	Tracking            int8 `firestore:"tracking" json:"tracking"`
}

type Skills struct {
	Agility      int8 `firestore:"agility" json:"agility"`
	Courage      int8 `firestore:"courage" json:"courage"`
	Endurance    int8 `firestore:"endurance" json:"endurance"`
	Intelligence int8 `firestore:"intelligence" json:"intelligence"`
	Perception   int8 `firestore:"perception" json:"perception"`
	Strength     int8 `firestore:"strength" json:"strength"`
}

type Equipment struct {
}

type Weapons struct {
}

// FetchUserSession fetches the user's session stored in Firestore - it contains the character definition and chosen theme
func FetchUserSession(ctx context.Context, client *firestore.Client, sessionID string) (*UserSessionType, error) {
	docRef := client.Collection("sessions").Doc(p.FirestoreSessionID)
	doc, err := docRef.Get(ctx)
	if err != nil {
		return nil, err
	}

	_ = doc.Data()
	var session UserSessionType
	if err := doc.DataTo(&session); err != nil {
		return nil, err
	}
	return &session, nil
}

// UpdateStoryState appends the new plot point and choice to Firestore
func UpdateStoryState(ctx context.Context, client *firestore.Client, sessionID string, newEvent string) error {
	docRef := client.Collection("sessions").Doc(p.FirestoreSessionID)

	_, err := docRef.Update(ctx, []firestore.Update{
		{
			Path:  "major_events",
			Value: firestore.ArrayUnion(newEvent),
		},
	})
	return err
}

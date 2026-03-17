# Story Context

**Theme:** {{.Theme}}

## Main Character Details

- **Name:** {{.CharacterProfile.Name}}
- **Race:** {{.CharacterProfile.Race}}
- **Gender:** {{.CharacterProfile.Gender}}
- **Body Type:** {{.CharacterProfile.BodyType}}
- **Feature:** {{.CharacterProfile.Feature}}
- **Clothes:** {{.CharacterProfile.Clothes}}

## Companion

- **Race:** {{.CompanionProfile.Race}}
- **Gender:** {{.CompanionProfile.Gender}}

## Skills

- Agility: {{.Skills.Agility}}
- Courage: {{.Skills.Courage}}
- Endurance: {{.Skills.Endurance}}
- Intelligence: {{.Skills.Intelligence}}
- Perception: {{.Skills.Perception}}
- Strength: {{.Skills.Strength}}

## Competences

- Animal Empathy: {{.Competences.AnimalEmpathy}}
- Celestial Navigation: {{.Competences.CelestialNavigation}}
- Critical Strike: {{.Competences.CriticalStrike}}
- Engineering: {{.Competences.Engineering}}
- Herbalism & Alchemy: {{.Competences.HerbalismAndAlchemy}}
- Intimidation: {{.Competences.Intimidation}}
- Linguistics: {{.Competences.Linguistics}}
- Shadow Blending: {{.Competences.ShadowBlending}}
- Tracking: {{.Competences.Tracking}}

## Equipment & Weapons

- **Equipment:** {{range .Equipment}}{{.}}, {{end}}
- **Weapons:** {{range .Weapons}}{{.}}, {{end}}

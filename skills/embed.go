package skills

import "embed"

// Embedded contains the portable Ooh Agent Skill distributed with the binary.
//
//go:embed ooh/SKILL.md ooh/agents/openai.yaml
var Embedded embed.FS

package main

// Embedded fallback assets: a freshly downloaded binary works standalone with
// the native free-tier body shape baked in. Disk files next to the binary, if
// present, always take precedence (loadResources reads them first) so operators
// can still customize prompts/tools without rebuilding.
import "embed"

//go:embed system_prompt_chat.txt system_prompt_muse.txt base_tools.json base_tools_muse.json
var assetFS embed.FS

//go:embed public/index.html
var indexFS embed.FS

# Project Analysis & Mapping

## 1. Core Architecture Discovery
- [x] Read `README.md` to understand high-level goals.
- [ ] Analyze `cmd/server/main.go` for entry point and route registration.
- [ ] Explore `internal/modules/providers` to map out account management and AI logic.
- [ ] Examine `internal/modules/mcp` for Model Context Protocol implementation.
- [ ] Review `internal/modules/gemini` for Deep Research and session handling.

## 2. Data & Configuration Mapping
- [ ] Check `data/` directory for schema, account, and platform JSON structures.
- [ ] Inspect `.env.example` for all configurable environment variables.

## 3. Automation & Recovery Analysis
- [ ] Study `internal/modules/providers/gemini_browser.go` and related for anti-bot logic.
- [ ] Analyze `ARCHITECTURE_SELF_HEALING.md` and implementation for JSPB schema recovery.

## 4. Final Summary
- [ ] Generate a Mermaid diagram of the system flow.
- [ ] Present the full project capability report to the user.

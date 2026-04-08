---
slug: gemini-web-api
description: Workspace instructions for gemini-web-multi-to-api project
applyTo:
  - "**/*.go"
  - "cmd/**"
  - "internal/**"
---

# Workspace Instructions: Gemini Web-to-API & MCP Proxy

## Overview

This is a sophisticated Go project that provides a reverse-engineered API for Gemini web, with advanced features including:
- **Multi-account pooling** with automatic rotation and rate-limit handling
- **Self-healing schema detection** for Google's JSPB payload structure changes
- **Anti-bot recovery** using browser automation (Rod library)
- **MCP (Model Context Protocol) native support** for AI agents
- **Multi-API compatibility**: OpenAI, Claude, and Gemini APIs

**Key Documentation**: See [ARCHITECTURE_SELF_HEALING.md](../ARCHITECTURE_SELF_HEALING.md) for detailed information on self-healing mechanisms and anti-bot recovery strategies.

---

## Project Structure & Conventions

### Build & Development Commands

Use **Task** (Taskfile.yml) for all build operations:

```bash
task swagger    # Generate Swagger documentation
task run        # Run server with auto-swagger update  
task dev        # Development mode
task build      # Build binary to ./gemini-web-to-api
```

**Go Version**: 1.25.1+

### Architecture: Dependency Injection (FX)

This project uses **go.uber.org/fx** for dependency injection. Module structure is strict and composable:

#### Module Pattern (Required)

Every module follows this structure:

```
internal/modules/{module_name}/
├── {module}_module.go      # FX dependency injection setup
├── {module}_controller.go  # HTTP handlers
├── {module}_service.go     # Business logic
├── dto/                    # Data transfer objects
└── [other_files]
```

**Module Registration Template**:
```go
package example

import (
	"github.com/gofiber/fiber/v3"
	"go.uber.org/fx"
)

var Module = fx.Options(
	fx.Provide(NewExampleService),
	fx.Provide(NewExampleController),
	fx.Invoke(RegisterRoutes),
)

func RegisterRoutes(app *fiber.App, c *ExampleController) {
	group := app.Group("/example")
	c.Register(group)
}
```

**Location**: All modules must be registered in [internal/modules/combine_module.go](../internal/modules/combine_module.go)

### Configuration System

**Config Structure**: [internal/commons/configs/configs.go](../internal/commons/configs/configs.go)

**Environment Variables** (see [.env.example](../.env.example)):
- `PORT=4982` - Server port
- `LOG_LEVEL=info` - Logging level
- `ADMIN_API_KEY` - Admin dashboard & internal auth
- `GEMINI_1PSID`, `GEMINI_1PSIDTS` - Gemini session cookies
- `GEMINI_REFRESH_INTERVAL=1440` - Account refresh in minutes
- `MCP_ENABLED=true` - Enable MCP protocol support
- `RATE_LIMIT_ENABLED=true`, `RATE_LIMIT_WINDOW_MS=60000`, `RATE_LIMIT_MAX_REQUESTS=30`

**Loading**: Config loads from `.env` via `godotenv`, errors during startup should be caught and logged.

### Logging Convention

**Framework**: go.uber.org/zap (structured logging)

**Pattern**:
```go
import "go.uber.org/zap"

// Constructor signature
func NewService(log *zap.Logger) *Service {
	log.Info("Service initialized", 
		zap.String("environment", "production"))
}

// Levels: log.Debug(), log.Info(), log.Warn(), log.Error()
```

**Logging Context**:
- All errors should include relevant context fields
- HTTP middleware automatically logs incoming requests at DEBUG level
- Self-healing recovery attempts should be logged at INFO/WARN level

---

## Key Technical Patterns

### 1. Self-Healing Schema Detection

**Context**: Google frequently changes JSPB payload structures.

**Mechanism**:
- When `ExtractFromPayload()` fails to extract response text despite HTTP 200, logs trigger `🚨 EXTRACTION FAILED. TRIGGERING AUTO-HEALING...`
- System uses a "Gemini Pro API key" Oracle (via `GEMINI_PRO_API_KEYS` from config)
- Sends minified JSPB payload to Oracle to recover new GJSON extraction paths
- Validates recovery with probe message `"hãy trả lời 'ok'"` (max 5 attempts)
- On success, saves schema to `SchemaManager`

**Implementation Location**: See [ARCHITECTURE_SELF_HEALING.md](../ARCHITECTURE_SELF_HEALING.md) - Section I

**When Adding Features**: If you modify payload extraction, ensure `ExtractFromPayload()` gracefully handles missing fields and triggers healing.

### 2. Anti-Bot Recovery (ClearBot)

**Context**: Google flags suspicious account activity as "Bot" (429 HTML or 403).

**Activation Triggers**:
- HTTP 429 with `<html>` tag (not JSON)
- HTTP 403 responses

**Recovery Process**:
1. Account moves to `StatusBanned` state
2. Browser automation opens headless Chromium via Rod
3. Simulates human behavior: mouse movement, clicking, scrolling
4. Types probe message and waits 60 seconds for response
5. Captures debug screenshot to `debug_clear_bot.png`
6. Sequential queue prevents resource exhaustion (only 1 browser at a time)

**Implementation Location**: See [ARCHITECTURE_SELF_HEALING.md](../ARCHITECTURE_SELF_HEALING.md) - Section II

**State Management**: 
- Account states: `Healthy`, `Banned`, `Error`
- Stored in `data/accounts.json`
- Browser profiles cached in `data/browser_profiles/{account_id}/`

### 3. Account Pool Management

**Files**:
- [internal/commons/models/models.go](../internal/commons/models/models.go) - Account data structures

**Key Features**:
- Automatic rotation between healthy accounts
- Rate limit detection and retry logic
- Heartbeat mechanism to keep sessions alive
- Pacing between requests to avoid detection

**When Adding Features**: Always check if account rotation needs to be updated.

### 4. Module Interop: Providers & Controllers

**Provider Pattern**: [internal/modules/providers/](../internal/modules/providers/)
- Abstract HTTP client configuration
- Used by Gemini, Claude, OpenAI modules

**Controller Pattern**: All controllers implement `Register(group *fiber.Group)` for route mounting
- Example: [internal/modules/gemini/gemini_controller.go](../internal/modules/gemini/gemini_controller.go)

---

## HTTP API Conventions

### Framework: Gofiber v3

**Middleware Stack** (in [internal/server/server.go](../internal/server/server.go)):
- CORS (allows `*` origins in development)
- Recovery middleware (panic recovery)
- Request logging at DEBUG level
- Rate limiting (configurable)
- Swagger UI at `/swagger/index.html`

### Route Structure

```
/admin/*              → Admin functions
/gemini/v1beta/*      → Gemini API endpoints  
/claude/v1/*          → Claude API endpoints
/openai/v1/*          → OpenAI compatibility
/mcp/*                → Model Context Protocol (SSE transport)
```

### Authorization Pattern

**Header**: `Authorization: Bearer <ADMIN_API_KEY>`

**Validation**: Performed per-route where needed (not globally)

---

## Docker & Deployment

### Build Process

Multi-stage Dockerfile:
1. **Build stage**: Go 1.25 Alpine builder
   - Generates Swagger docs via `swag init`
   - CGO disabled, stripped binary
2. **Runtime stage**: Alpine 3.22.2
   - Includes Chromium (for anti-bot recovery)
   - Non-root user (`appuser:appgroup`)
   - Init system `tini` for zombie process management

### Environment Setup

**Port**: 4982 (exposed)

**Required for ClearBot**:
- Chromium browser installed
- `/usr/bin/chromium-browser` on Linux
- Sufficient memory (minimum ~500MB free for browser + app)

### Compose Support

Use [docker-compose.yml](../docker-compose.yml) with overrides for development

---

## Testing & Validation

### Swagger Documentation

Auto-generated from code comments (Swaggo format). Update with:
```bash
task swagger
```

### Manual Testing

**Admin API Key**: Set `ADMIN_API_KEY` in `.env` to test admin endpoints

**Test Endpoints**:
- `/gemini/v1/chat/completions` - Gemini chat
- `/claude/v1/messages` - Claude API
- `/openai/v1/chat/completions` - OpenAI compatibility
- `/mcp/sse` - MCP protocol (establish SSE connection)

---

## Common Pitfalls & Troubleshooting

### Schema Healing Issues

**Problem**: Recovery attempts fail or hang
- **Check**: Are `GEMINI_PRO_API_KEYS` valid and not rate-limited?
- **Timeout**: ClearBot has 120s timeout for Chrome startup
- See [ARCHITECTURE_SELF_HEALING.md](../ARCHITECTURE_SELF_HEALING.md) - Section III for error codes

### Anti-Bot Recovery Not Triggering

**Problem**: Banned accounts not being recovered
- **Check**: `StatusBanned` accounts in `data/accounts.json`
- **Memory**: Ensure sufficient RAM for Chromium (~300-500MB per attempt)
- **Sequential Queue**: Only 1 browser runs at a time; check if queue is blocked

### Rate Limiting on Accounts

**Problem**: Too many 429 errors
- **Solution**: Add more accounts to pool or increase `GEMINI_REFRESH_INTERVAL`
- **Rotation**: Automatic but can be manually triggered via `/admin` endpoints

### Docker Build Failures

**Problem**: Chromium installation fails in Alpine
- **Solution**: Ensure base Alpine 3.22.2+, includes proper library dependencies
- **Check**: Docker build logs for missing packages (nss, freetype, dbus, etc.)

---

## Adding New Features

### Adding a New Module

1. Create directory: `internal/modules/{new_module}/`
2. Implement module.go with FX options
3. Implement controller(s) with `Register()` method
4. Implement service(s) with business logic
5. Create `dto/` directory for request/response models
6. Register in [internal/modules/combine_module.go](../internal/modules/combine_module.go)

### Adding New Endpoints

1. Create controller method
2. Add Swaggo comment for documentation
3. Mount route in `RegisterRoutes()`
4. Run `task swagger` to regenerate docs

### Accessing Configuration

```go
func NewExampleService(cfg *configs.Config) *ExampleService {
	return &ExampleService{
		geminiCfg: cfg.Gemini,
		logLevel: cfg.LogLevel,
	}
}
```

### Adding Logging

```go
import "go.uber.org/zap"

func (s *Service) DoSomething(log *zap.Logger) error {
	log.Info("Starting operation", 
		zap.String("action", "process"),
		zap.Int("count", 42))
	
	if err != nil {
		log.Error("Operation failed", 
			zap.Error(err),
			zap.String("action", "process"))
		// Don't forget to return error
	}
}
```

---

## Key Files to Know

| File | Purpose |
|------|---------|
| [cmd/server/main.go](../cmd/server/main.go) | App entry point, FX setup |
| [internal/server/server.go](../internal/server/server.go) | Fiber app configuration |
| [internal/commons/configs/configs.go](../internal/commons/configs/configs.go) | Configuration management |
| [internal/modules/gemini/gemini_service.go](../internal/modules/gemini/gemini_service.go) | Gemini API core logic |
| [internal/modules/combine_module.go](../internal/modules/combine_module.go) | Module registration |
| [ARCHITECTURE_SELF_HEALING.md](../ARCHITECTURE_SELF_HEALING.md) | Deep technical details |
| [taskfile.yml](../taskfile.yml) | Build commands |
| [.env.example](../.env.example) | Configuration template |

---

## Development Workflow

### Setup

1. Install Go 1.25+
2. Copy `.env.example` to `.env`
3. Configure `ADMIN_API_KEY` and Gemini cookies
4. Run: `go mod download`

### Local Development

```bash
# Start with auto-swagger generation
task dev

# Or run directly
go run cmd/server/main.go

# Build for deployment
task build
```

### Before Committing

```bash
# Regenerate docs
task swagger

# Run tests (if applicable)
go test ./...

# Check formatting
go fmt ./...
```

---

## Performance Considerations

- **Browser Automation**: ClearBot uses sequential queue to avoid resource exhaustion
- **Rate Limiting**: Configure via `RATE_LIMIT_*` variables for API protection
- **Schema Caching**: Self-healing results cached in SchemaManager (in-memory)
- **Account Rotation**: Automatic pacing prevents hitting Google's anti-bot detection

---

**Last Updated**: 2026-04-05  
**Maintainer**: Antigravity AI Assistant  
**Documentation**: See [README.md](../README.md) for user-facing guide

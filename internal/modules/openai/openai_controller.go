package openai

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"time"

	models "gemini-web-to-api/internal/commons/models"
	utils "gemini-web-to-api/internal/commons/utils"
	"gemini-web-to-api/internal/modules/openai/dto"
	"gemini-web-to-api/internal/modules/providers"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"
)

type OpenAIController struct {
	service *OpenAIService
	log     *zap.Logger
}

func NewOpenAIController(service *OpenAIService) *OpenAIController {
	return &OpenAIController{
		service: service,
		log:     zap.NewNop(),
	}
}

// SetLogger sets the logger for this handler
func (h *OpenAIController) SetLogger(log *zap.Logger) {
	h.log = log
}

// GetModelData returns raw model data for internal use (e.g. unified list)
func (h *OpenAIController) GetModelData() []models.ModelData {
	availableModels := h.service.ListModels()

	var data []models.ModelData
	for _, m := range availableModels {
		data = append(data, models.ModelData{
			ID:      m.ID,
			Object:  "model",
			Created: m.Created,
			OwnedBy: m.OwnedBy,
		})
	}
	return data
}

// HandleModels returns the list of supported models
// @Summary List OpenAI Models
// @Description Returns a list of models supported by the OpenAI-compatible API
// @Tags OpenAI
// @Accept json
// @Produce json
// @Success 200 {object} models.ModelListResponse
// @Router /openai/v1/models [get]
func (h *OpenAIController) HandleModels(c fiber.Ctx) error {
	data := h.GetModelData()

	return c.JSON(models.ModelListResponse{
		Object: "list",
		Data:   data,
	})
}

// HandleChatCompletions accepts requests in OpenAI format
// @Summary Chat Completions (OpenAI)
// @Description Generates a completion for the chat message
// @Tags OpenAI
// @Accept json
// @Produce json
// @Param request body dto.ChatCompletionRequest true "Chat Completion Request"
// @Success 200 {object} dto.ChatCompletionResponse
// @Failure 400 {object} map[string]interface{}
// @Failure 500 {object} map[string]interface{}
// @Router /openai/v1/chat/completions [post]
func (h *OpenAIController) HandleChatCompletions(c fiber.Ctx) error {
	var req dto.ChatCompletionRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(fmt.Errorf("invalid request body: %w", err), "invalid_request_error"))
	}

	// Add timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	response, err := h.service.CreateChatCompletion(ctx, req)
	if err != nil {
		h.log.Error("GenerateContent failed", zap.Error(err), zap.String("model", req.Model))
		
		status := fiber.StatusInternalServerError
		errorType := "api_error"
		if errors.Is(err, providers.ErrAllAccountsExhausted) {
			status = fiber.StatusServiceUnavailable
			errorType = "service_unavailable"
		}
		
		errResp := utils.ErrorToResponse(err, errorType)
		h.log.Info("Sending error response to client", zap.Int("status", status), zap.Any("error", errResp))
		return c.Status(status).JSON(errResp)
	}

	if req.Stream {
		c.Set("Content-Type", "text/event-stream")
		c.Set("Cache-Control", "no-cache")
		c.Set("Connection", "keep-alive")

		c.Response().SetBodyStreamWriter(func(w *bufio.Writer) {
			id := response.ID
			created := response.Created
			model := response.Model

			// Send role delta first
			roleChunk := dto.ChatCompletionChunk{
				ID:      id,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []dto.ChunkChoice{
					{
						Index: 0,
						Delta: models.Delta{Role: "assistant"},
					},
				},
			}
			utils.SendSSEEvent(w, h.log, roleChunk)

			// If the response contains tool calls, send them in a single chunk
			if len(response.Choices[0].Message.ToolCalls) > 0 {
				toolChunk := dto.ChatCompletionChunk{
					ID:      id,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []dto.ChunkChoice{
						{
							Index: 0,
							Delta: models.Delta{
								ToolCalls: response.Choices[0].Message.ToolCalls,
							},
						},
					},
				}
				utils.SendSSEEvent(w, h.log, toolChunk)
			} else {
				// Split the generated text and stream it
				chunks := utils.SplitResponseIntoChunks(response.Choices[0].Message.Content, 0)
				for _, textChunk := range chunks {
					dataChunk := dto.ChatCompletionChunk{
						ID:      id,
						Object:  "chat.completion.chunk",
						Created: created,
						Model:   model,
						Choices: []dto.ChunkChoice{
							{
								Index: 0,
								Delta: models.Delta{Content: textChunk},
							},
						},
					}
					if !utils.SendSSEEvent(w, h.log, dataChunk) {
						break // Stop if connection is closed or writing fails
					}
					time.Sleep(10 * time.Millisecond) // Simulating network latency
				}
			}

			// Send final chunk with finish_reason
			finalChunk := dto.ChatCompletionChunk{
				ID:      id,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []dto.ChunkChoice{
					{
						Index:        0,
						Delta:        models.Delta{},
						FinishReason: response.Choices[0].FinishReason,
					},
				},
			}
			utils.SendSSEEvent(w, h.log, finalChunk)

			// Send DONE marker
			fmt.Fprintf(w, "data: [DONE]\n\n")
			w.Flush()
		})
		return nil
	}

	c.Set("Content-Type", "application/json")
	return c.JSON(response)
}

func (h *OpenAIController) convertToOpenAIFormat(response *providers.Response, model string) dto.ChatCompletionResponse {
	return dto.ChatCompletionResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().Unix()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []dto.Choice{
			{
				Index: 0,
				Message: models.Message{
					Role:    "assistant",
					Content: response.Text,
				},
				FinishReason: "stop",
			},
		},
		Usage: models.Usage{
			PromptTokens:     0,
			CompletionTokens: 0,
			TotalTokens:      0,
		},
	}
}

// Register registers the OpenAI routes onto the provided group
func (c *OpenAIController) Register(group fiber.Router) {
	group.Get("/models", c.HandleModels)
	group.Post("/chat/completions", c.HandleChatCompletions)
	group.Post("/images/generations", c.HandleImageGenerations)
}

// HandleImageGenerations accepts requests in OpenAI format for image generation
// @Summary Image Generations (OpenAI)
// @Description Creates an image given a prompt
// @Tags OpenAI
// @Accept json
// @Produce json
// @Param request body dto.ImageGenerationRequest true "Image Generation Request"
// @Success 200 {object} dto.ImageGenerationResponse
// @Failure 400 {object} map[string]interface{}
// @Failure 500 {object} map[string]interface{}
// @Router /openai/v1/images/generations [post]
func (h *OpenAIController) HandleImageGenerations(c fiber.Ctx) error {
	var req dto.ImageGenerationRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(fmt.Errorf("invalid request body: %w", err), "invalid_request_error"))
	}

	// Default values
	if req.N == 0 {
		req.N = 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	response, err := h.service.CreateImageGeneration(ctx, req)
	if err != nil {
		h.log.Error("Image generation failed", zap.Error(err), zap.String("prompt", req.Prompt))
		
		status := fiber.StatusInternalServerError
		errorType := "api_error"
		if errors.Is(err, providers.ErrAllAccountsExhausted) {
			status = fiber.StatusServiceUnavailable
			errorType = "service_unavailable"
		}
		
		errResp := utils.ErrorToResponse(err, errorType)
		return c.Status(status).JSON(errResp)
	}

	c.Set("Content-Type", "application/json")
	return c.JSON(response)
}

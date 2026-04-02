package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gemini-web-to-api/internal/commons/configs"

	"go.uber.org/zap"
)

type AccountStatus string

const (
	StatusHealthy AccountStatus = "Healthy"
	StatusError   AccountStatus = "Error"
	StatusBanned  AccountStatus = "Banned"
)

var ErrAllAccountsExhausted = errors.New("all Gemini accounts exhausted or timed out")

type AccountConfig struct {
	ID            string        `json:"id"`
	Secure1PSID   string        `json:"__Secure-1PSID"`
	Secure1PSIDTS string        `json:"__Secure-1PSIDTS"`
	Status        AccountStatus `json:"status"`
	SuccessCount  int           `json:"success_count"`
	ErrorCount    int           `json:"error_count"`
	TargetIP      string        `json:"target_ip"`
	AddedAt       int64         `json:"added_at"`
	UpdatedAt     int64         `json:"updated_at"`
}

// Client acts as an AccountManager and a LoadBalancer across multiple Gemini Web sessions.
// It provides centralized worker acquisition with queue support to ensure each account
// only processes one request at a time across ALL services (OpenAI, Claude, Gemini, MCP).
type Client struct {
	cfg         *configs.Config
	log         *zap.Logger
	storagePath string

	workers      []*Worker
	accountsMap  map[string]*AccountConfig
	mu           sync.RWMutex
	cond         *sync.Cond // signals when a worker becomes idle
	currentIndex int
}

func NewClient(cfg *configs.Config, log *zap.Logger) *Client {
	storagePath := "data/accounts.json"
	os.MkdirAll("data", 0755)

	c := &Client{
		cfg:         cfg,
		log:         log,
		storagePath: storagePath,
		workers:     []*Worker{},
		accountsMap: make(map[string]*AccountConfig),
	}
	c.cond = sync.NewCond(&c.mu)

	return c
}

func (c *Client) Init(ctx context.Context) error {
	c.LoadAccounts()

	// If there's an element in .env, ensure it exists in accounts
	if c.cfg.Gemini.Secure1PSID != "" {
		envID := "env-default"
		if _, exists := c.accountsMap[envID]; !exists {
			c.AddAccount(envID, c.cfg.Gemini.Secure1PSID, c.cfg.Gemini.Secure1PSIDTS)
		}
	}

	c.mu.Lock()
	for _, acc := range c.accountsMap {
		worker := NewWorker(c.cfg, c.log, acc.Secure1PSID, acc.Secure1PSIDTS)
		worker.AccountID = acc.ID
		worker.OnSuccess = c.ReportSuccess
		worker.OnError = c.ReportError
		// When a worker finishes, wake up all waiters in the queue
		worker.OnRelease = func() {
			c.cond.Broadcast()
		}

		go func(w *Worker, act *AccountConfig) {
			err := w.Init(context.Background())
			c.mu.Lock()
			if err != nil {
				act.Status = StatusError
				act.ErrorCount++
				c.log.Error("Worker init failed", zap.String("id", act.ID), zap.Error(err))
			} else {
				act.Status = StatusHealthy
			}
			act.UpdatedAt = time.Now().Unix()
			c.mu.Unlock()
			c.cond.Broadcast() // Wake up waiters — a new worker may be available
			c.SaveAccounts()
		}(worker, acc)

		c.workers = append(c.workers, worker)
	}
	c.mu.Unlock()

	go c.masterSync()

	return nil
}

func (c *Client) masterSync() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		c.mu.Lock()
		for _, w := range c.workers {
			if act, ok := c.accountsMap[w.AccountID]; ok {
				cks := w.GetCookies()
				act.Secure1PSID = cks.Secure1PSID
				act.Secure1PSIDTS = cks.Secure1PSIDTS
				act.UpdatedAt = cks.UpdatedAt.Unix()

				// Sync the worker's healthy state to the dashboard!
				if !w.IsHealthy() && act.Status != StatusBanned {
					act.Status = StatusError
				} else if w.IsHealthy() && act.Status != StatusHealthy {
					act.Status = StatusHealthy
					c.cond.Broadcast() // Wake up waiting requests
				}
			}
		}
		c.mu.Unlock()
		c.SaveAccounts()
	}
}

func (c *Client) LoadAccounts() {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := os.ReadFile(c.storagePath)
	if err != nil {
		return
	}

	var accounts []AccountConfig
	if err := json.Unmarshal(data, &accounts); err == nil {
		for i := range accounts {
			c.accountsMap[accounts[i].ID] = &accounts[i]
		}
	}
}

func (c *Client) SaveAccounts() {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var accounts []AccountConfig
	for _, acc := range c.accountsMap {
		accounts = append(accounts, *acc)
	}

	data, _ := json.MarshalIndent(accounts, "", "  ")
	os.WriteFile(c.storagePath, data, 0644)
}

func (c *Client) AddAccount(id, psid, psidts string) {
	c.mu.Lock()

	acc := &AccountConfig{
		ID:            id,
		Secure1PSID:   psid,
		Secure1PSIDTS: psidts,
		Status:        StatusHealthy,
		AddedAt:       time.Now().Unix(),
		UpdatedAt:     time.Now().Unix(),
	}
	c.accountsMap[id] = acc

	worker := NewWorker(c.cfg, c.log, psid, psidts)
	worker.AccountID = id
	worker.OnRelease = func() {
		c.cond.Broadcast()
	}
	c.workers = append(c.workers, worker)
	c.mu.Unlock()

	go func() {
		err := worker.Init(context.Background())
		c.mu.Lock()
		if err != nil {
			acc.Status = StatusError
		} else {
			acc.Status = StatusHealthy
		}
		c.mu.Unlock()
		c.cond.Broadcast()
		c.SaveAccounts()
	}()
	c.SaveAccounts()
}

func (c *Client) RemoveAccount(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.accountsMap, id)

	for i, w := range c.workers {
		if w.AccountID == id {
			w.Close()
			c.workers = append(c.workers[:i], c.workers[i+1:]...)
			break
		}
	}
	go c.SaveAccounts()
}

func (c *Client) GetAccounts() []AccountConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var accounts []AccountConfig
	for _, acc := range c.accountsMap {
		accounts = append(accounts, *acc)
	}
	return accounts
}

func (c *Client) TestAccount(id string) error {
	c.mu.RLock()
	var targetWorker *Worker
	for _, w := range c.workers {
		if w.AccountID == id {
			targetWorker = w
			break
		}
	}
	c.mu.RUnlock()

	if targetWorker == nil {
		return fmt.Errorf("account not found in active pool")
	}

	// Run the connection test which hits Google to refresh tokens
	err := targetWorker.TestConnection()

	c.mu.Lock()
	if acc, ok := c.accountsMap[id]; ok {
		if err != nil {
			acc.Status = StatusError
			acc.ErrorCount++
		} else {
			if acc.Status != StatusHealthy {
				acc.Status = StatusHealthy
				c.cond.Broadcast() // Wake up waiting requests
			}
		}
		acc.UpdatedAt = time.Now().Unix()
	}
	c.mu.Unlock()

	go c.SaveAccounts()
	return err
}

// findIdleWorkerLocked scans workers round-robin for an idle+healthy+non-banned worker.
// Caller MUST hold c.mu (Lock, not RLock).
// Returns (worker, account, error). If no idle worker found, returns (nil, nil, nil).
func (c *Client) findIdleWorkerLocked() (*Worker, *AccountConfig, error) {
	if len(c.workers) == 0 {
		return nil, nil, fmt.Errorf("no accounts configured")
	}

	startIdx := c.currentIndex
	for i := 0; i < len(c.workers); i++ {
		idx := (startIdx + i) % len(c.workers)
		w := c.workers[idx]

		acc, ok := c.accountsMap[w.AccountID]
		if !ok {
			continue
		}

		if w.IsHealthy() && !w.IsBusy() && acc.Status != StatusBanned {
			c.currentIndex = (idx + 1) % len(c.workers)
			w.SetBusy(true) // Atomically mark as busy before releasing lock
			return w, acc, nil
		}
	}

	return nil, nil, nil // No error, just none available right now
}

// AcquireWorker blocks until an idle+healthy worker is available or ctx expires.
// Returns the worker, account config, and a release function.
// Caller MUST call the release function when done with the worker.
// This is the SINGLE centralized entry point for all services to get a worker.
func (c *Client) AcquireWorker(ctx context.Context) (*Worker, *AccountConfig, func(), error) {
	c.mu.Lock()

	if len(c.workers) == 0 {
		c.mu.Unlock()
		return nil, nil, nil, fmt.Errorf("no accounts configured")
	}

	for {
		// Check context before each attempt
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return nil, nil, nil, err
		}

		// Check if anyone can possibly service this (at least one non-banned worker)
		eligibleFound := false
		for _, w := range c.workers {
			if a, ok := c.accountsMap[w.AccountID]; ok && a.Status != StatusBanned {
				eligibleFound = true
				break
			}
		}
		if !eligibleFound {
			c.mu.Unlock()
			return nil, nil, nil, ErrAllAccountsExhausted
		}

		worker, acc, err := c.findIdleWorkerLocked()
		if err != nil {
			c.mu.Unlock()
			return nil, nil, nil, err
		}
		if worker != nil {
			// Ensure session is fresh before every request
			if err := worker.RefreshSession(); err != nil {
				c.log.Warn("⚠️ Failed to refresh session before request", zap.String("account_id", acc.ID), zap.Error(err))
				// Continue to find another worker or retry since this one's session initiation failed
				worker.SetBusy(false)
				continue
			}

			c.mu.Unlock()

			// Build release function — must be called exactly once by the caller
			released := false
			releaseFunc := func() {
				if released {
					return // Idempotent
				}
				released = true
				worker.SetBusy(false)
				c.cond.Broadcast() // Wake up other waiters
			}

			c.log.Info("🔓 Worker acquired",
				zap.String("account_id", acc.ID),
			)
			return worker, acc, releaseFunc, nil
		}

		// All workers busy — wait in queue
		c.log.Debug("⏳ All accounts busy, waiting in queue...")

		// Spawn goroutine to wake us if context is cancelled
		ctxDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				c.cond.Broadcast() // Wake up the Wait() below
			case <-ctxDone:
				// Normal path — we got a worker or gave up
			}
		}()

		c.cond.Wait() // Releases c.mu, sleeps, re-acquires c.mu on wake
		close(ctxDone) // Clean up context watcher goroutine
	}
}

// GenerateContent acquires an idle worker, generates content, and releases the worker.
// This is used by Claude, Gemini native, MCP, and image generation — all stateless single-shot calls.
func (c *Client) GenerateContent(ctx context.Context, prompt string, options ...GenerateOption) (*Response, error) {
	c.mu.RLock()
	maxAttempts := len(c.workers)
	c.mu.RUnlock()

	if maxAttempts == 0 {
		return nil, fmt.Errorf("no accounts configured")
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Acquire an idle worker (blocks if all busy)
		worker, acc, release, err := c.AcquireWorker(ctx)
		if err != nil {
			return nil, err
		}

		// 60s timeout for each account attempt
		workerCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		c.log.Info("📡 SENDING TO GEMINI...", zap.String("account_id", acc.ID), zap.Int("attempt", attempt+1))
		resp, err := worker.GenerateContent(workerCtx, prompt, options...)
		cancel()
		release() // Always release after each attempt

		if err == nil {
			c.log.Info("✅ GEMINI SUCCESS", zap.String("account_id", acc.ID))
			return resp, nil
		}

		// Handle fatal account errors: 401, 403
		isFatal := strings.Contains(err.Error(), "status 403") || strings.Contains(err.Error(), "status 401") || strings.Contains(err.Error(), "blocked")

		if isFatal {
			c.log.Error("❌ FATAL: Account Access Denied / Banned", zap.String("account_id", acc.ID), zap.Error(err))
			if attempt < maxAttempts-1 {
				c.log.Info("🔄 FALLBACK: Selecting next healthy account after cooldown...", zap.Int("next_attempt", attempt+2))

				// Cooldown: Wait a bit before hitting the next account to avoid IP-based rate limiting
				select {
				case <-time.After(2 * time.Second):
					continue // Try next worker
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return nil, err
		}

		// Prevent cascading bans on prompt block or safety filter (don't rotate accounts if the PROMPT is the issue)
		if errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrSafetyBlock) {
			c.log.Warn("⚠️ GEMINI [Safety/Access] ERROR: Prompt block or safety filter triggered. NOT falling back to protect pool pool.", zap.String("account_id", acc.ID), zap.Error(err))
			return nil, fmt.Errorf("gemini rejected request (safety block or constraint): %w", err)
		}

		// Handle timeout errors (retryable)
		isTimeout := errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded")
		if !isTimeout {
			c.log.Error("❌ ERROR: Worker failed with unknown non-retryable error", zap.String("account_id", acc.ID), zap.Error(err))
			return nil, err
		}

		lastErr = err
		c.log.Warn("⚠️ GEMINI TIMEOUT: Worker timed out, trying next account...", zap.String("account_id", acc.ID), zap.Int("attempt", attempt+1), zap.Error(err))

		// If the main context was canceled by the client, stop trying
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}

	c.log.Error("❌ GEMINI ERROR: All Gemini accounts exhausted or timed out", zap.Error(lastErr))
	return nil, ErrAllAccountsExhausted
}

func (c *Client) GetWorkerCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.workers)
}

// StartChatWithContext acquires an idle worker, creates a ChatSession on it,
// and returns the session + a release function. Caller MUST call release() when done.
// This replaces the old StartChat() for callers that need proper queue integration.
func (c *Client) StartChatWithContext(ctx context.Context, options ...ChatOption) (ChatSession, func(), error) {
	worker, acc, release, err := c.AcquireWorker(ctx)
	if err != nil {
		return nil, nil, err
	}

	c.log.Info("💬 StartChat: Worker acquired for chat session", zap.String("account_id", acc.ID))
	session := worker.StartChat(options...)
	return session, release, nil
}

// StartChat is a convenience wrapper that acquires a worker with a 30s timeout.
// DEPRECATED: prefer StartChatWithContext for proper queue integration.
func (c *Client) StartChat(options ...ChatOption) ChatSession {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, _, err := c.StartChatWithContext(ctx, options...)
	if err != nil {
		c.log.Warn("StartChat failed to acquire worker", zap.Error(err))
		return nil
	}
	// Note: release is NOT called here — the session worker stays busy until
	// Worker.GenerateContent or SendMessage internally manages it.
	// This is intentionally a legacy path.
	return session
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.workers {
		w.Close()
	}
	return nil
}

func (c *Client) GetName() string {
	return "gemini-loadbalancer"
}

func (c *Client) IsHealthy() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, w := range c.workers {
		if w.IsHealthy() {
			return true
		}
	}
	return false
}

func (c *Client) ListModels() []ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()

	modelMap := make(map[string]ModelInfo)
	for _, w := range c.workers {
		for _, m := range w.ListModels() {
			modelMap[m.ID] = m
		}
	}

	var models []ModelInfo
	for _, m := range modelMap {
		models = append(models, m)
	}
	return models
}

func (c *Client) ListModelsIDs() []string {
	models := c.ListModels()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

func (c *Client) ReportSuccess(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if acc, ok := c.accountsMap[id]; ok {
		acc.SuccessCount++
		if acc.Status != StatusHealthy {
			acc.Status = StatusHealthy
			c.cond.Broadcast() // Wake up waiting requests
		}
		acc.UpdatedAt = time.Now().Unix()
		go c.SaveAccounts()
	}
}

func (c *Client) ReportError(id string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if acc, ok := c.accountsMap[id]; ok {
		acc.ErrorCount++
		if err != nil && (strings.Contains(err.Error(), "status 401") || strings.Contains(err.Error(), "status 403")) {
			acc.Status = StatusBanned
		} else {
			acc.Status = StatusError
		}
		acc.UpdatedAt = time.Now().Unix()
		go c.SaveAccounts()
	}
}

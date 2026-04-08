package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"gemini-web-to-api/internal/commons/configs"

	req "github.com/imroc/req/v3"

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

	// Bot Clearance Queue
	botClearQueue     chan string
	highPriorityQueue chan string
	botPendingMap     sync.Map // prevents duplicate queueing
	isHealing         bool
	healingMu         sync.Mutex
	healingStatus     string
	schemaMgr         *SchemaManager
	multiGuestMgr     *MultiGuestManager
	guestWorkers      map[string]*GuestWorker // Map of platform name -> worker
	discoverySvc      *DiscoveryService
}

func NewClient(cfg *configs.Config, log *zap.Logger) *Client {
	storagePath := "data/accounts.json"
	os.MkdirAll("data", 0755)

	c := &Client{
		cfg:               cfg,
		log:               log,
		storagePath:       storagePath,
		workers:           []*Worker{},
		accountsMap:       make(map[string]*AccountConfig),
		schemaMgr:         NewSchemaManager(),
		multiGuestMgr:     NewMultiGuestManager(),
		guestWorkers:      make(map[string]*GuestWorker),
		healingStatus:     "Hoạt động bình thường",
		botClearQueue:     make(chan string, 100), // Buffer for up to 100 pending clears
		highPriorityQueue: make(chan string, 10),  // High priority (Guest)
	}
	c.cond = sync.NewCond(&c.mu)

	// Discovery Service
	c.discoverySvc = NewDiscoveryService(cfg, log, c.multiGuestMgr, c)

	// Create initial guest workers from existing configs
	for _, pc := range c.multiGuestMgr.GetValidConfigs() {
		w := NewGuestWorker(cfg, log, c)
		w.PlatformName = pc.Name
		c.guestWorkers[pc.Name] = w
	}

	// Start Sequential Bot Worker
	go c.startBotWorker()

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
		worker.SetSchemaManager(c.schemaMgr)
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

	// Initialize guest workers
	for _, gw := range c.guestWorkers {
		_ = gw.Init(ctx)
	}

	go c.masterSync()

	// Initial pool health check (Ensures gemini exists and triggers discovery if needed)
	c.CheckGuestPoolHealth()

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

		// Simplified Guest Check: Always ensure Gemini Guest is bootstrapped
		c.CheckGuestPoolHealth()
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
	worker.SetSchemaManager(c.schemaMgr)
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
	if strings.HasPrefix(id, "guest-") {
		platform := strings.TrimPrefix(id, "guest-")
		c.log.Info("♻️ Admin requested to REMOVE GUEST platform", zap.String("platform", platform))
		c.multiGuestMgr.RemoveConfig(platform)

		// Also remove worker if exists
		c.mu.Lock()
		delete(c.guestWorkers, platform)
		c.mu.Unlock()
		return
	}

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

	// Add synthetic Guest Accounts
	for name, pc := range c.multiGuestMgr.Configs {
		status := StatusHealthy
		if pc.Disabled {
			status = "Disabled"
		} else if !pc.IsValid {
			status = StatusError
		}

		guestAcc := AccountConfig{
			ID:            "guest-" + name,
			Secure1PSID:   "GUEST_MODE (" + name + ")",
			Secure1PSIDTS: "Learned: " + time.Unix(pc.LastLearned, 0).Format("2006-01-02 15:04"),
			Status:        status,
			AddedAt:       pc.LastLearned,
			UpdatedAt:     time.Now().Unix(),
		}
		accounts = append(accounts, guestAcc)
	}

	return accounts
}

func (c *Client) TestAccount(id string) error {
	if strings.HasPrefix(id, "guest-") {
		return c.TestGuest(strings.TrimPrefix(id, "guest-"))
	}

	c.mu.Lock()
	var targetWorker *Worker
	for _, w := range c.workers {
		if w.AccountID == id {
			targetWorker = w
			break
		}
	}

	acc, ok := c.accountsMap[id]
	if !ok {
		c.mu.Unlock()
		return fmt.Errorf("account %s not found", id)
	}

	// If worker not found but account exists, create worker
	isNew := false
	if targetWorker == nil {
		isNew = true
		c.log.Info("🚀 Worker for account not in pool, creating new worker for test", zap.String("id", id))
		targetWorker = NewWorker(c.cfg, c.log, acc.Secure1PSID, acc.Secure1PSIDTS)
		targetWorker.AccountID = id
		targetWorker.OnSuccess = c.ReportSuccess
		targetWorker.OnError = c.ReportError
		targetWorker.SetSchemaManager(c.schemaMgr)
		targetWorker.OnRelease = func() {
			c.cond.Broadcast()
		}
		c.workers = append(c.workers, targetWorker)
	}
	c.mu.Unlock()

	// Run the connection test (hits Google to refresh tokens)
	var err error
	if isNew {
		err = targetWorker.Init(context.Background())
	} else {
		err = targetWorker.TestConnection()
	}

	c.mu.Lock()
	// Re-get account in case it was modified during async call
	if acc, ok = c.accountsMap[id]; ok {
		if err != nil {
			// Nếu tài khoản đang bị Disabled thì giữ nguyên Disabled, nếu không thì báo lỗi Error
			if acc.Status != "Disabled" {
				acc.Status = StatusError
			}
			acc.ErrorCount++
		} else {
			// Test thành công! Chuyển sang Healthy và kích hoạt lại worker
			if acc.Status != StatusHealthy {
				acc.Status = StatusHealthy
				c.cond.Broadcast() // Wake up waiting requests
			}
			// Đảm bảo vòng lặp auto-refresh hoạt động
			targetWorker.Reactivate()
		}
		acc.UpdatedAt = time.Now().Unix()
	}
	c.mu.Unlock()

	go c.SaveAccounts()
	return err
}

func (c *Client) TestGuest(platform string) error {
	gw, ok := c.GetGuestWorker(platform)
	if !ok {
		// Try to create it if config exists
		if _, exists := c.multiGuestMgr.GetConfig(platform); exists {
			gw = NewGuestWorker(c.cfg, c.log, c)
			gw.PlatformName = platform
			c.mu.Lock()
			c.guestWorkers[platform] = gw
			c.mu.Unlock()
		} else {
			return fmt.Errorf("guest platform %s not found", platform)
		}
	}

	c.log.Info("📡 SENDING HELLO TO GUEST...", zap.String("platform", platform))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := gw.GenerateContent(ctx, "hello")
	if err != nil {
		c.log.Error("❌ Guest test failed, triggering automatic re-learning...", zap.String("platform", platform), zap.Error(err))
		c.ResetGuest(platform)
		return fmt.Errorf("guest test 'hello' failed and re-learning triggered: %w", err)
	}

	if resp == nil || resp.Text == "" {
		return fmt.Errorf("guest test 'hello' returned empty response")
	}

	c.log.Info("✅ Guest test success", zap.String("platform", platform))
	return nil
}

func (c *Client) ResetGuest(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.guestWorkers, name)
	c.multiGuestMgr.Reset(name)
	c.log.Info("🔄 Guest platform access reset", zap.String("platform", name))
}

func (c *Client) ResetAllGuests() {
	configs := c.multiGuestMgr.GetConfigs()

	// Sort by name for consistent queuing order
	var names []string
	for _, pc := range configs {
		if !pc.Disabled {
			names = append(names, pc.Name)
		}
	}

	// Sort names
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[i] > names[j] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}

	for _, name := range names {
		c.ResetGuest(name)
	}
}

// CheckGuestPoolHealth triggers discovery if active (non-disabled) guest count is below target
func (c *Client) CheckGuestPoolHealth() {
	// Always ensure 'gemini' platform exists in the list as it's the primary fallback
	if pc, ok := c.multiGuestMgr.GetConfig("gemini"); !ok || (!pc.IsValid && !pc.Disabled) {
		if !ok {
			c.log.Info("🚀 Pool missing 'gemini'. Bootstrapping primary guest...")
			c.multiGuestMgr.SaveConfig(PlatformConfig{
				Name:    "gemini",
				BaseURL: "https://gemini.google.com/app",
				IsValid: false,
			})
		}
		c.TriggerGuestLearning("gemini")
	}
}

// findIdleWorkerLocked scans workers round-robin for an idle+healthy+non-banned worker.
// Caller MUST hold c.mu (Lock, not RLock).
// Returns (worker, account, error). If no idle worker found, returns (nil, nil, nil).
func (c *Client) findIdleWorkerLocked() (*Worker, *AccountConfig, error) {
	if len(c.workers) == 0 {
		return nil, nil, fmt.Errorf("no accounts configured")
	}

	// Pick a random starting index to satisfy the "random account" requirement
	startIdx := rand.Intn(len(c.workers))
	for i := 0; i < len(c.workers); i++ {
		idx := (startIdx + i) % len(c.workers)
		w := c.workers[idx]

		acc, ok := c.accountsMap[w.AccountID]
		if !ok {
			continue
		}

		if w.IsHealthy() && !w.IsBusy() && acc.Status != StatusBanned && acc.Status != "Disabled" {
			// Update currentIndex just for legacy tracking if needed, though we now use random
			c.currentIndex = (idx + 1) % len(c.workers)
			w.SetBusy(true) // Atomically mark as busy before releasing lock
			return w, acc, nil
		}
	}

	return nil, nil, nil // No error, just none available right now
}

// findIdleWorkerExcludingLocked is like findIdleWorkerLocked but skips accounts in excludeIDs.
// Used for retry-with-different-account logic. Caller MUST hold c.mu.
func (c *Client) findIdleWorkerExcludingLocked(excludeIDs map[string]bool) (*Worker, *AccountConfig, error) {
	if len(c.workers) == 0 {
		return nil, nil, fmt.Errorf("no accounts configured")
	}

	startIdx := rand.Intn(len(c.workers))
	for i := 0; i < len(c.workers); i++ {
		idx := (startIdx + i) % len(c.workers)
		w := c.workers[idx]

		if excludeIDs[w.AccountID] {
			continue // Skip already-tried accounts
		}

		acc, ok := c.accountsMap[w.AccountID]
		if !ok {
			continue
		}

		if w.IsHealthy() && !w.IsBusy() && acc.Status != StatusBanned && acc.Status != "Disabled" {
			c.currentIndex = (idx + 1) % len(c.workers)
			w.SetBusy(true)
			return w, acc, nil
		}
	}

	return nil, nil, nil
}

// AcquireWorkerExcluding picks an idle worker that is NOT in excludeIDs.
// NON-BLOCKING: returns ErrAllAccountsExhausted immediately if nothing is available.
// Used for retry loops that need to try a DIFFERENT account on each attempt.
func (c *Client) AcquireWorkerExcluding(ctx context.Context, excludeIDs map[string]bool) (*Worker, *AccountConfig, func(), error) {
	if ctx.Err() != nil {
		return nil, nil, nil, ctx.Err()
	}

	c.mu.Lock()
	worker, acc, err := c.findIdleWorkerExcludingLocked(excludeIDs)
	if err != nil {
		c.mu.Unlock()
		return nil, nil, nil, err
	}
	if worker == nil {
		c.mu.Unlock()
		return nil, nil, nil, ErrAllAccountsExhausted
	}
	c.mu.Unlock()

	// RefreshSession was removed to avoid excessive requests to Google.
	// The background auto-refresh loop handles token maintenance.
	// If the session is truly dead, GenerateContent will fail and trigger retry.

	released := false
	releaseFunc := func() {
		if released {
			return
		}
		released = true
		worker.SetBusy(false)
		c.cond.Broadcast()
	}

	c.log.Info("🔓 Worker acquired (excluding)", zap.String("account_id", acc.ID), zap.Int("already_tried", len(excludeIDs)))
	return worker, acc, releaseFunc, nil
}

// AcquireWorker blocks until an idle+healthy worker is available or ctx expires.
// Returns the worker, account config, and a release function.
// Caller MUST call the release function when done with the worker.
// This is the SINGLE centralized entry point for all services to get a worker.
func (c *Client) AcquireWorker(ctx context.Context) (*Worker, *AccountConfig, func(), error) {
	c.mu.Lock()

	// Check if any regular accounts are healthy or even configured
	if len(c.workers) == 0 {
		return c.acquireGuestFallbackLocked(ctx)
	}

	for {
		// Check context before each attempt
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return nil, nil, nil, err
		}

		// Check if anyone can possibly service this (at least one worker that is healthy or busy)
		eligibleFound := false
		for _, w := range c.workers {
			if a, ok := c.accountsMap[w.AccountID]; ok && a.Status != StatusBanned {
				// We can only wait for an account if it is either healthy (ready to be picked but maybe busy)
				// or busy (so it will eventually free up). If it's neither, waiting is useless.
				if w.IsHealthy() || w.IsBusy() {
					eligibleFound = true
					break
				}
			}
		}
		if !eligibleFound {
			// All regular accounts are banned or none configured
			return c.acquireGuestFallbackLocked(ctx)
		}

		worker, acc, err := c.findIdleWorkerLocked()
		if err != nil {
			c.mu.Unlock()
			return nil, nil, nil, err
		}
		if worker != nil {
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

		c.cond.Wait()  // Releases c.mu, sleeps, re-acquires c.mu on wake
		close(ctxDone) // Clean up context watcher goroutine
	}
}

func (c *Client) acquireGuestFallbackLocked(_ context.Context) (*Worker, *AccountConfig, func(), error) {
	c.log.Info("⚠️ Initializing Guest fallback...", zap.Int("workers", len(c.workers)))

	// 1. Collect and Sort Valid Guest Workers (Priority: Gemini first, then others)
	var validGuests []*GuestWorker

	// Always try gemini first if it exists and is valid
	if g, exists := c.guestWorkers["gemini"]; exists && g.GetConfig().IsValid {
		validGuests = append(validGuests, g)
	}

	// Get other valid guests
	for name, g := range c.guestWorkers {
		if name != "gemini" && g.GetConfig().IsValid {
			validGuests = append(validGuests, g)
		}
	}

	if len(validGuests) > 0 {
		// 2. Select Guest (Priority 1: Gemini, then Round Robin for the rest)
		var selected *GuestWorker

		// If only one, take it. If multiple, rotate.
		if len(validGuests) == 1 {
			selected = validGuests[0]
		} else {
			// Chọn ngẫu nhiên Guest để tránh việc dồn request vào các guest ở đầu danh sách
			idx := rand.Intn(len(validGuests))
			selected = validGuests[idx]
		}

		if selected != nil {
			pc := selected.GetConfig()
			c.mu.Unlock()
			schema := pc.GJSONPaths
			mockWorker := &Worker{
				AccountID:  "guest-" + selected.PlatformName,
				httpClient: selected.httpClient,
				log:        selected.log,
				healthy:    true,
				at:         pc.AtToken,
				SchemaMgr:  &SchemaManager{schema: &schema},
			}
			mockAcc := &AccountConfig{ID: "guest-" + selected.PlatformName}
			return mockWorker, mockAcc, func() {}, nil
		}
	}

	c.mu.Unlock()
	return nil, nil, nil, ErrAllAccountsExhausted
}

// AcquireGuestWorker explicitly skips all regular accounts and fetches a Guest Worker
func (c *Client) AcquireGuestWorker(ctx context.Context) (*Worker, *AccountConfig, func(), error) {
	c.mu.Lock()
	return c.acquireGuestFallbackLocked(ctx)
}

// GenerateContent acquires an idle worker, generates content, and releases the worker.
// This is used by Claude, Gemini native, MCP, and image generation — all stateless single-shot calls.
func (c *Client) GenerateContent(ctx context.Context, prompt string, options ...GenerateOption) (*Response, error) {

	// 0. Fail-fast if healing is in progress
	c.healingMu.Lock()
	if c.isHealing {
		status := c.healingStatus
		c.healingMu.Unlock()
		return nil, fmt.Errorf("hệ thống đang trong quá trình tự chữa lành (Self-Healing). Trạng thái: %s. Vui lòng thử lại sau ít phút.", status)
	}
	c.healingMu.Unlock()

	maxRetries := c.cfg.Gemini.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	var lastErr error
	useGuest := false

	// Try regular account workers first
	if len(c.workers) > 0 {
		for attempt := 0; attempt <= maxRetries; attempt++ {
			// Acquire an idle worker (blocks if all busy)
			worker, acc, release, err := c.AcquireWorker(ctx)
			if err != nil {
				// If no workers available at all, fallback to guest
				if errors.Is(err, ErrAllAccountsExhausted) {
					c.log.Warn("⚠️ No regular accounts available, falling back to guest system")
					useGuest = true
					break
				}
				return nil, err
			}

			// Initial wait is 60s (thinking time), then it drops to 30s after first byte
			initialStaleDuration := 60 * time.Second
			pingStaleDuration := 30 * time.Second
			currentStaleDuration := initialStaleDuration
			staleTimer := time.NewTimer(currentStaleDuration)
			
			// Custom context for this worker attempt so we can kill it on stale timeout
			workerCtx, cancel := context.WithCancel(ctx)
			
			// Wrap options with our progress tracker
			finalOptions := append(options, WithProgress(func() {
				// Ping-pong: Reset the stale timer whenever data is received
				if !staleTimer.Stop() {
					select {
					case <-staleTimer.C:
					default:
					}
				}
				// After first byte, use the shorter 30s stale timeout
				if currentStaleDuration != pingStaleDuration {
					currentStaleDuration = pingStaleDuration
					c.log.Debug("⚡ First byte received! Switching to 30s stale timeout.", zap.String("account_id", acc.ID))
				}
				staleTimer.Reset(currentStaleDuration)
				c.log.Debug("🏓 PING-PONG: Data chunk received, timeout reset", zap.String("account_id", acc.ID), zap.Duration("timeout", currentStaleDuration))
			}))

			c.log.Info("📡 SENDING TO GEMINI...", zap.String("account_id", acc.ID), zap.Int("attempt", attempt+1))
			
			type result struct {
				resp *Response
				err  error
			}
			resChan := make(chan result, 1)

			go func() {
				resp, err := worker.GenerateContent(workerCtx, prompt, finalOptions...)
				resChan <- result{resp, err}
			}()

			var resp *Response
			select {
			case res := <-resChan:
				resp = res.resp
				err = res.err
				staleTimer.Stop()
			case <-staleTimer.C:
				c.log.Warn("⌛ STALE TIMEOUT: No data received for a while. Killing session.", zap.String("account_id", acc.ID), zap.Duration("limit", currentStaleDuration))
				cancel() // Kill the worker's HTTP request
				err = fmt.Errorf("stale timeout: no progress for %v", currentStaleDuration)
			case <-ctx.Done():
				staleTimer.Stop()
				cancel()
				release()
				return nil, ctx.Err()
			}
			
			cancel()
			release() // Always release after each attempt

			if err == nil {
				c.log.Info("✅ GEMINI SUCCESS", zap.String("account_id", acc.ID))
				return resp, nil
			}

			lastErr = err

			// Handle anti-bot or structure error: SHIFT to guest immediately
			isStructureError := strings.Contains(err.Error(), "failed to parse response")
			isFatal := strings.Contains(err.Error(), "status 403") || strings.Contains(err.Error(), "status 401") ||
				strings.Contains(err.Error(), "status 429") || strings.Contains(err.Error(), "blocked") ||
				errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrBotBlocked)

			if isFatal || isStructureError {
				if isFatal {
					c.log.Error("❌ FATAL: Account Anti-bot triggered", zap.String("account_id", acc.ID), zap.Error(err))
					c.ReportError(acc.ID, err)
					c.TriggerBotClearance(acc.ID)
				} else {
					c.log.Warn("🚨 EXTRACTION FAILED (Structure Error)", zap.String("account_id", acc.ID))
					go func() {
						healCtx, cancelH := context.WithTimeout(context.Background(), 5*time.Minute)
						defer cancelH()
						c.RunHealing(healCtx)
					}()
				}

				c.log.Warn("⚠️ Anti-bot/Structure error detected. Skipping other ID cookies and jumping to Guest.")
				useGuest = true
				break
			}

			// Handle safety block (don't retry, don't fallback to guest - it's the prompt)
			if errors.Is(err, ErrSafetyBlock) {
				c.log.Warn("⚠️ GEMINI [Safety] ERROR: Prompt block. Terminating request.", zap.String("account_id", acc.ID))
				return nil, fmt.Errorf("gemini rejected request (safety block): %w", err)
			}

			// Handle timeout: retry with next random account if matches maxRetries
			isTimeout := errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded") || strings.Contains(err.Error(), "stale timeout")
			if isTimeout {
				c.log.Warn("⚠️ GEMINI TIMEOUT: Trying next random account...", zap.String("account_id", acc.ID), zap.Int("attempt", attempt+1))
				if attempt == maxRetries {
					c.log.Info("🔄 Max retries reached for timeouts. Falling back to guest.")
					useGuest = true
					break
				}
				// Otherwise continues loop to next attempt
				continue
			}

			// Other non-retryable errors
			c.log.Error("❌ ERROR: Worker failed with non-retryable error", zap.String("account_id", acc.ID), zap.Error(err))
			return nil, err
		}
	} else {
		useGuest = true
	}

	// Guest Fallback path
	if useGuest {
		c.log.Info("⚡ Executing Guest Fallback (Load Balanced)...")
		worker, acc, release, err := c.AcquireGuestWorker(ctx)
		if err == nil {
			// Apply the same 60s initial / 30s stale logic to Guest Gemini
			initialStaleDuration := 60 * time.Second
			pingStaleDuration := 30 * time.Second
			currentStaleDuration := initialStaleDuration
			staleTimer := time.NewTimer(currentStaleDuration)

			workerCtx, cancel := context.WithCancel(ctx)
			
			finalOptions := append(options, WithProgress(func() {
				if !staleTimer.Stop() {
					select {
					case <-staleTimer.C:
					default:
					}
				}
				if currentStaleDuration != pingStaleDuration {
					currentStaleDuration = pingStaleDuration
					c.log.Debug("⚡ Guest First byte received! Switching to 30s stale timeout.", zap.String("guest_id", acc.ID))
				}
				staleTimer.Reset(currentStaleDuration)
				c.log.Debug("🏓 Guest PING-PONG: Data chunk received, timeout reset", zap.String("guest_id", acc.ID), zap.Duration("timeout", currentStaleDuration))
			}))

			c.log.Info("📡 SENDING TO GUEST GEMINI...", zap.String("guest_id", acc.ID))

			type result struct {
				resp *Response
				err  error
			}
			resChan := make(chan result, 1)

			go func() {
				resp, err := worker.GenerateContent(workerCtx, prompt, finalOptions...)
				resChan <- result{resp, err}
			}()

			var resp *Response
			var gErr error

			select {
			case res := <-resChan:
				resp = res.resp
				gErr = res.err
				staleTimer.Stop()
			case <-staleTimer.C:
				c.log.Warn("⌛ STALE TIMEOUT: Guest Gemini no progress for a while. Killing session.", zap.String("guest_id", acc.ID), zap.Duration("limit", currentStaleDuration))
				cancel()
				gErr = fmt.Errorf("guest stale timeout: no progress for %v", currentStaleDuration)
			case <-ctx.Done():
				staleTimer.Stop()
				cancel()
				release()
				return nil, ctx.Err()
			}

			cancel()
			release()

			if gErr == nil {
				c.log.Info("✅ GUEST SUCCESS", zap.String("guest_id", acc.ID))
				return resp, nil
			}

			// If guest fails, report and return final error
			c.HandleGuestError(acc.ID, gErr)
			c.log.Error("❌ GUEST FAILED", zap.String("guest_id", acc.ID), zap.Error(gErr))
			return nil, fmt.Errorf("guest system also failed: %w", gErr)
		}

		if lastErr != nil {
			return nil, fmt.Errorf("all primary accounts failed (last: %v) and guest system is unavailable: %v", lastErr, err)
		}
		return nil, fmt.Errorf("guest system unavailable: %w", err)
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrAllAccountsExhausted
}

func (c *Client) GetWorkerCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	count := len(c.workers)
	for _, gw := range c.guestWorkers {
		if gw.IsHealthy() {
			count++
		}
	}
	return count
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

	// Also add models from guest workers
	for _, gw := range c.guestWorkers {
		for _, m := range gw.ListModels() {
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
		if err != nil {
			isDeadCookie := strings.Contains(err.Error(), "cookie likely dead")
			isBanned := strings.Contains(err.Error(), "status 401") || strings.Contains(err.Error(), "status 403") || strings.Contains(err.Error(), "status 429") || errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrBotBlocked)

			if isDeadCookie {
				c.log.Warn("🚫 Account ID Cookie is completely DEAD. Disabling immediately to prevent further ban cascade.", zap.String("account_id", id))
				acc.Status = "Disabled"

				// Stop the background refresh loop
				for _, w := range c.workers {
					if w.AccountID == id {
						w.Close()
						break
					}
				}
			} else if isBanned {
				acc.Status = StatusBanned

				// If it's a 401/403/429 or bot flag, trigger ClearBot
				c.TriggerBotClearance(id)
			} else {
				acc.Status = StatusError
			}
		} else {
			acc.Status = StatusError
		}
		acc.UpdatedAt = time.Now().Unix()
		go c.SaveAccounts()
	}
}

func (c *Client) GetSchemaMgr() *SchemaManager {
	return c.schemaMgr
}

func (c *Client) GetMultiGuestMgr() *MultiGuestManager {
	return c.multiGuestMgr
}

func (c *Client) GetDiscoverySvc() *DiscoveryService {
	return c.discoverySvc
}

func (c *Client) GetGuestWorker(name string) (*GuestWorker, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w, ok := c.guestWorkers[name]
	if !ok {
		// Try to create from config if not in map
		if pc, exists := c.multiGuestMgr.GetConfig(name); exists && pc.IsValid {
			w = NewGuestWorker(c.cfg, c.log, c)
			w.PlatformName = pc.Name
			c.guestWorkers[pc.Name] = w
			c.log.Info("🆕 Dynamically registered GuestWorker", zap.String("platform", name))
			return w, true
		}
	}
	return w, ok
}

func (c *Client) IsHealing() bool {
	c.healingMu.Lock()
	defer c.healingMu.Unlock()
	return c.isHealing
}

func (c *Client) GetHealingStatus() string {
	c.healingMu.Lock()
	defer c.healingMu.Unlock()
	if !c.isHealing {
		return "Hoạt động bình thường"
	}
	return c.healingStatus
}

// RunHealing starts the automated recovery process
func (c *Client) RunHealing(ctx context.Context) error {
	c.healingMu.Lock()
	if c.isHealing {
		c.healingMu.Unlock()
		return nil // Already healing
	}
	c.isHealing = true
	c.healingStatus = "Bắt đầu quy trình tự chữa lành..."
	c.healingMu.Unlock()

	defer func() {
		c.healingMu.Lock()
		c.isHealing = false
		c.healingStatus = "Hoạt động bình thường"
		c.healingMu.Unlock()
	}()

	c.log.Info("🏥 STARTING AUTO-HEALING PROCESS...")

	// 1. Get Ground Truth from a worker
	worker, _, release, err := c.AcquireWorker(ctx)
	if err != nil {
		return err
	}
	defer release()

	// Special probe message
	probeContent := "hãy trả lời 'ok'"

	// Temporarily enable raw logging for this probe if not already
	oldLogRaw := c.cfg.Gemini.LogRawRequests
	c.cfg.Gemini.LogRawRequests = true
	defer func() { c.cfg.Gemini.LogRawRequests = oldLogRaw }()

	// Loop for maximum 5 attempts to find a working schema
	maxRetry := 5
	for i := 1; i <= maxRetry; i++ {
		c.healingMu.Lock()
		c.healingStatus = fmt.Sprintf("Thử nghiệm lần %d: Đang gửi 'hãy trả lời ok' đến Gemini Web...", i)
		c.healingMu.Unlock()

		c.log.Info("🏥 Healing Attempt", zap.Int("attempt", i))

		// 1. Get a fresh ground truth from Gemini Web
		resp, err := worker.GenerateContent(ctx, probeContent)
		var rawResponse string
		if resp != nil && resp.Metadata != nil {
			if raw, ok := resp.Metadata["raw_payload"].(string); ok {
				rawResponse = raw
			}
		}

		if rawResponse == "" {
			c.log.Warn("⚠️ No raw response captured from Gemini Web, retrying...", zap.Error(err))
			continue
		}

		// Extract inner payloads from the raw response stream
		innerPayloads := ExtractInnerPayloads(rawResponse)
		if len(innerPayloads) == 0 {
			c.log.Warn("⚠️ No inner payloads found in raw response, retrying...")
			continue
		}

		// Pick the payload that contains "ok"
		oracleInput := innerPayloads[len(innerPayloads)-1]
		for _, p := range innerPayloads {
			if strings.Contains(strings.ToLower(p), "ok") {
				oracleInput = p
				break
			}
		}

		c.healingMu.Lock()
		c.healingStatus = fmt.Sprintf("Thử nghiệm lần %d: Đang gửi dữ liệu thô cho Oracle tìm vị trí 'ok'...", i)
		c.healingMu.Unlock()

		c.log.Info("🔍 Captured Raw Ground-Truth Payload", zap.Int("size", len(oracleInput)))

		// 2. Consult Oracle (Iterate through all keys)
		newSchema, err := c.callOracle(ctx, "ok", oracleInput)
		if err != nil {
			c.log.Warn("❌ Oracle consultation failed", zap.Error(err))
			if strings.Contains(err.Error(), "429") {
				c.log.Error("🛑 All Oracle API keys are rate limited (429). Stopping healing.")
				return err
			}
			continue
		}

		// 3. VALIDATION: Perform a FRESH request to Gemini Web to verify schema
		c.healingMu.Lock()
		c.healingStatus = fmt.Sprintf("Thử nghiệm lần %d: Đã có schema gợi ý, đang thực hiện request thực tế để xác nhận...", i)
		c.healingMu.Unlock()

		c.log.Info("🧪 Validating suggestion with a FRESH request to Gemini Web...")
		verifyResp, vErr := worker.GenerateContent(ctx, probeContent)
		if vErr != nil && !strings.Contains(vErr.Error(), "failed to parse") {
			c.log.Warn("⚠️ Verification request failed (network/auth), retrying...", zap.Error(vErr))
			continue
		}

		// Extract using the new schema from the fresh response
		if verifyResp != nil && verifyResp.Metadata != nil {
			if freshRaw, ok := verifyResp.Metadata["raw_payload"].(string); ok {
				verifyPayloads := ExtractInnerPayloads(freshRaw)
				validated := false
				for _, p := range verifyPayloads {
					var payload []interface{}
					if json.Unmarshal([]byte(p), &payload) == nil {
						text, _, _, _, exErr := ExtractFromPayload(payload, *newSchema)
						if exErr == nil && strings.TrimSpace(strings.ToLower(text)) == "ok" {
							validated = true
							break
						}
					}
				}

				if validated {
					c.log.Info("✅ VALIDATION SUCCESS! Schema works on fresh response.")
					c.schemaMgr.UpdateSchema(newSchema)
					c.log.Info("🎉 AUTO-HEALING COMPLETE!")
					return nil
				}
				c.log.Warn("❌ Validation failed on fresh response payloads")
			}
		}
	}

	return errors.New("failed to find working schema after 5 attempts")
}

// GenerateWithOracle is a dedicated path for system tasks (like Discovery) that uses
// raw Gemini API keys (Google Generative AI) instead of account-based workers.
// It bypasses the worker queue and uses a randomized try-all strategy across all API keys.
func (c *Client) GenerateWithOracle(ctx context.Context, prompt string) (*Response, error) {
	rawKeys := c.cfg.Gemini.OracleAPIKeys
	rawKeys = strings.Trim(rawKeys, "[] ")
	apiKeys := strings.Split(rawKeys, ",")
	for i := range apiKeys {
		apiKeys[i] = strings.Trim(apiKeys[i], " \"'")
	}

	if len(apiKeys) == 0 || (len(apiKeys) == 1 && apiKeys[0] == "") {
		return nil, errors.New("no Gemini API keys (Oracle) configured in .env (GEMINI_PRO_API_KEYS)")
	}

	payload := map[string]interface{}{
		"contents": []interface{}{
			map[string]interface{}{
				"parts": []interface{}{
					map[string]interface{}{
						"text": prompt,
					},
				},
			},
		},
	}

	// CRITICAL: DO NOT CHANGE THIS MODEL! 'gemini-flash-latest' is essential for system stability.
	// This model is used for guest discovery and path analysis because of its fast response
	// and high compliance with non-conversational instructions.
	// Changing this will break the autonomous guest lifecycle.
	// =========================================================================================
	const OracleModel = "gemini-flash-latest"
	apiURL := "https://generativelanguage.googleapis.com/v1beta/models/" + OracleModel + ":generateContent"

	// Shuffle keys to distribute load
	shuffledKeys := make([]string, len(apiKeys))
	copy(shuffledKeys, apiKeys)
	rand.Shuffle(len(shuffledKeys), func(i, j int) { shuffledKeys[i], shuffledKeys[j] = shuffledKeys[j], shuffledKeys[i] })

	var lastErr error
	for idx, apiKey := range shuffledKeys {
		if apiKey == "" {
			continue
		}
		c.log.Info("🔮 Oracle Generation: Attempting with API key", zap.Int("key_index", idx), zap.String("key_prefix", apiKey[:8]))

		oracleClient := req.NewClient().SetTimeout(90 * time.Second)
		resp, err := oracleClient.R().
			SetContext(ctx).
			SetHeader("X-goog-api-key", apiKey).
			SetHeader("Content-Type", "application/json").
			SetBody(payload).
			Post(apiURL)

		if err != nil {
			lastErr = err
			continue
		}

		if !resp.IsSuccess() {
			lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, resp.String())
			continue
		}

		var result struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}

		if err := json.Unmarshal(resp.Bytes(), &result); err != nil {
			lastErr = err
			continue
		}

		if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
			lastErr = errors.New("empty oracle response")
			continue
		}

		text := result.Candidates[0].Content.Parts[0].Text
		return &Response{Text: text, Candidates: []Candidate{{Content: text}}}, nil
	}

	return nil, fmt.Errorf("oracle generation failed after trying all %d keys. last error: %w", len(apiKeys), lastErr)
}

// callOracle is the specialized path for JSON path analysis (JSPB minification + GJSON schema extraction)
func (c *Client) callOracle(ctx context.Context, expectedValue string, rawContent string) (*ExtractorSchema, error) {
	rawKeys := c.cfg.Gemini.OracleAPIKeys
	// Normalize if user provided JSON-like [ "key1", "key2" ]
	rawKeys = strings.Trim(rawKeys, "[] ")
	apiKeys := []string{}
	for _, k := range strings.Split(rawKeys, ",") {
		k = strings.TrimSpace(strings.Trim(k, " \"'"))
		if k != "" {
			apiKeys = append(apiKeys, k)
		}
	}

	if len(apiKeys) == 0 {
		return nil, errors.New("no Oracle API Keys configured (GEMINI_PRO_API_KEYS)")
	}

	// Xáo trộn ngẫu nhiên các Key để tránh dùng chết các Key ở đầu danh sách
	rand.New(rand.NewSource(time.Now().UnixNano())).Shuffle(len(apiKeys), func(i, j int) {
		apiKeys[i], apiKeys[j] = apiKeys[j], apiKeys[i]
	})

	// Tắt tự viết regex thủ công, dùng package json chuẩn để nén an toàn:
	minifiedBody := minifyJSPB(rawContent)

	c.log.Info("🔮 Oracle: Sending JSPB for analysis...",
		zap.Int("input_len", len(rawContent)),
		zap.Int("minified_len", len(minifiedBody)),
		zap.String("expected", expectedValue))

	prompt := fmt.Sprintf(`I sent a probe message to Gemini Web and received the following raw nested JSON array response (JSPB format).
I need to find the correct GJSON paths to extract the response text, which should contain the phrase "%s" (case-insensitive).

Raw Response JSON (Minified, null=0 to save context):
%s

Your task is to structurally trace the nested array indices to find where "%s" is located.

CRITICAL INSTRUCTIONS FOR GJSON PATHS:
Our Go code extracts the text using this exact logic:
1. `+"`"+`candidates := gjson.Get(rawPayload, schema.CandidatePath)`+"`"+` -> Retrieves the array of candidates. (Common candidate_path examples: "4", "5", "6", or "0.4")
2. `+"`"+`firstCandidate := candidates.Array()[0]`+"`"+` -> Gets the very first element (index 0) from that array.
3. `+"`"+`text = gjson.Get(firstCandidate.Raw, schema.TextPath).String()`+"`"+` -> Applies text_path RELATIVE TO THE FIRST ELEMENT. (Common text_path: "1.0", "1.1", or "2.0")

Example derivation:
If text is at root[4][0][1][0], then:
- candidate_path is "4" (root[4] is the array of candidates)
- first item is index 0 (stripped implicitly)
- text_path is "1.0" (relative to root[4][0])

Indices to identify:
- rcid_path: path to a string like "rc_..." (Relative to first candidate, common: "0")
- cid_path: path to conversation ID (Relative to the ROOT payload, common: "1")
- rid_path: path to response ID (Relative to the ROOT payload, common: "2")

Return ONLY a valid JSON object with these keys:
{
  "candidate_path": "...",
  "text_path": "...",
  "rcid_path": "...",
  "cid_path": "...",
  "rid_path": "..."
}`, expectedValue, minifiedBody, expectedValue)

	payload := map[string]interface{}{
		"contents": []interface{}{
			map[string]interface{}{
				"parts": []interface{}{
					map[string]interface{}{
						"text": prompt,
					},
				},
			},
		},
		"generationConfig": map[string]interface{}{
			"responseMimeType": "application/json",
		},
	}

	// CRITICAL: DO NOT CHANGE THIS MODEL! 'gemini-flash-latest' is specifically configured
	// for stable extraction performance and JSON mode compatibility in this system.
	// Changing it to any other model (like gemini-pro) may cause structured extraction failures
	// or unexpected response formats.
	// =========================================================================================
	const OracleModel = "gemini-flash-latest"
	apiURL := "https://generativelanguage.googleapis.com/v1beta/models/" + OracleModel + ":generateContent"

	// Shuffle keys to distribute load and ensure high availability
	shuffledKeys := make([]string, len(apiKeys))
	copy(shuffledKeys, apiKeys)
	rand.Shuffle(len(shuffledKeys), func(i, j int) { shuffledKeys[i], shuffledKeys[j] = shuffledKeys[j], shuffledKeys[i] })

	// Try all keys sequentially until one succeeds
	var lastErr error
	allRetryable := true

	for idx, apiKey := range shuffledKeys {
		c.log.Info("🔮 Oracle Attempt: Consultation with Gemini API", zap.Int("key_index", idx), zap.String("key_prefix", apiKey[:8]))

		oracleClient := req.NewClient().SetTimeout(120 * time.Second)
		c.log.Debug("📤 Sending minified JSPB to Oracle...", zap.Int("payload_length", len(minifiedBody)))

		resp, err := oracleClient.R().
			SetContext(ctx).
			SetHeader("X-goog-api-key", apiKey).
			SetHeader("Content-Type", "application/json").
			SetBody(payload).
			Post(apiURL)

		if err != nil {
			lastErr = err
			allRetryable = false
			continue
		}

		if resp.StatusCode == 429 || resp.StatusCode == 403 {
			reason := "rate limited (429)"
			if resp.StatusCode == 403 {
				reason = "permission denied/suspended (403)"
			}
			c.log.Warn(fmt.Sprintf("⚠️ Key %s, trying next key...", reason))
			continue
		}

		allRetryable = false
		if !resp.IsSuccess() {
			lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, resp.String())
			continue
		}

		var oracleResp struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}

		if err := json.Unmarshal(resp.Bytes(), &oracleResp); err != nil {
			lastErr = err
			continue
		}

		if len(oracleResp.Candidates) == 0 || len(oracleResp.Candidates[0].Content.Parts) == 0 {
			lastErr = errors.New("empty oracle response")
			continue
		}

		responseText := oracleResp.Candidates[0].Content.Parts[0].Text
		c.log.Info("🔮 Oracle Response received", zap.String("content", responseText))

		var schema ExtractorSchema
		if err := json.Unmarshal([]byte(responseText), &schema); err != nil {
			// Try to find JSON in markdown
			if strings.Contains(responseText, "```") {
				re := regexp.MustCompile("(?s)```(?:json)?\n(.*?)\n```")
				if match := re.FindStringSubmatch(responseText); len(match) > 1 {
					if err := json.Unmarshal([]byte(match[1]), &schema); err == nil {
						return &schema, nil
					}
				}
			}
			c.log.Warn("⚠️ Oracle returned invalid schema format", zap.String("responseText", responseText), zap.Error(err))
			lastErr = err
			continue
		}

		return &schema, nil
	}

	if allRetryable {
		return nil, errors.New("all Oracle API keys were either rate limited (429) or suspended (403)")
	}
	return nil, fmt.Errorf("oracle consultation failed after trying all %d keys. last error: %w", len(apiKeys), lastErr)
}

// minifyJSPB compresses valid JSON strings safely and replaces null with 0 to save tokens.
func minifyJSPB(input string) string {
	input = strings.TrimSpace(input)
	var out bytes.Buffer
	if err := json.Compact(&out, []byte(input)); err != nil {
		// If input is not valid JSON, just replace globally
		minified := strings.ReplaceAll(input, " ", "")
		minified = strings.ReplaceAll(minified, "\n", "")
		minified = strings.ReplaceAll(minified, "null", "0")
		return minified
	}

	// If it is valid JSON, after compaction, replace "null" with "0" safely
	// using strings.ReplaceAll on the compacted string.
	// This might technically catch "null" inside strings,
	// but for our structural search, it's acceptable.
	minified := out.String()
	minified = strings.ReplaceAll(minified, "null", "0")
	return minified
}

func (c *Client) TriggerBotClearance(accountID string) {
	if _, loaded := c.botPendingMap.LoadOrStore(accountID, true); !loaded {
		// Reroute guest fallback instances directly to the relearning mechanism
		if strings.HasPrefix(accountID, "guest-") && !strings.HasPrefix(accountID, "guest_relearn:") {
			platform := strings.TrimPrefix(accountID, "guest-")
			c.log.Info("🔄 Redirecting guest account ban to guest relearning process", zap.String("platform", platform))
			c.botPendingMap.Delete(accountID) // remove original ID since we redirect
			c.TriggerGuestLearning(platform)
			return
		}

		// For disabled guests: auto-reset if this is the ONLY option (i.e., all primary accounts also failing)
		// This prevents permanent deadlock when both primary accounts and guest platforms are disabled.
		if strings.HasPrefix(accountID, "guest_relearn:") {
			platform := strings.TrimPrefix(accountID, "guest_relearn:")
			if pc, ok := c.multiGuestMgr.GetConfig(platform); ok && pc.Disabled {
				// Check if any primary accounts are healthy — if not, force-reset the guest
				c.mu.RLock()
				anyHealthy := false
				for _, w := range c.workers {
					if w.IsHealthy() {
						anyHealthy = true
						break
					}
				}
				c.mu.RUnlock()
				if !anyHealthy {
					c.log.Warn("⚠️ Guest is DISABLED but no primary accounts are healthy — forcing guest reset to restore service", zap.String("platform", platform))
					c.multiGuestMgr.Reset(platform) // Reset: is_valid=false, fail_count=0, disabled=false
					// Fall through to allow re-learning
				} else {
					c.log.Info("🚫 Skipping re-learn for DISABLED guest (primary accounts still healthy)", zap.String("platform", platform))
					c.botPendingMap.Delete(accountID)
					return
				}
			}
		}

		c.log.Info("📋 Task added to queue", zap.String("id", accountID))

		// Prioritize Guest Learning to restore fallback service ASAP
		if strings.HasPrefix(accountID, "guest_relearn:") {
			select {
			case c.highPriorityQueue <- accountID:
				c.log.Info("🚀 HIGH-PRIORITY: Guest re-learning queued", zap.String("id", accountID))
			default:
				c.log.Warn("⚠️ High-priority queue full, skipping", zap.String("id", accountID))
				c.botPendingMap.Delete(accountID)
			}
			return
		}

		select {
		case c.botClearQueue <- accountID:
		default:
			c.log.Warn("⚠️ Bot Clearance Queue is full, skipping", zap.String("id", accountID))
			c.botPendingMap.Delete(accountID)
		}
	}
}

// TriggerGuestLearning adds the guest learning task to the sequential queue
func (c *Client) TriggerGuestLearning(platform string) {
	if platform == "" {
		platform = "gemini"
	}
	// Kiểm tra xem đã đến lúc thử lại chưa
	if pc, ok := c.multiGuestMgr.GetConfig(platform); ok {
		now := time.Now().Unix()
		if now < pc.NextAttemptAt {
			c.log.Debug("⏳ Guest re-learning throttled", 
				zap.String("platform", platform), 
				zap.Int64("wait_seconds", pc.NextAttemptAt - now))
			return
		}
	}
	c.TriggerBotClearance("guest_relearn:" + platform)
}

// startBotWorker runs in background and processes accounts one-by-one
func (c *Client) startBotWorker() {
	c.log.Info("👷 Bot Clearance Worker started (Strict Sequential mode)")
	for {
		var accountID string
		var isHighPriority bool

		// 1. Always check high-priority queue first
		select {
		case id := <-c.highPriorityQueue:
			accountID = id
			isHighPriority = true
		default:
			// 2. If no high-priority, wait for either pool (preferring high if both arrive)
			select {
			case id := <-c.highPriorityQueue:
				accountID = id
				isHighPriority = true
			case id := <-c.botClearQueue:
				accountID = id
				isHighPriority = false
			}
		}

		c.log.Info("🔄 Bot Worker: Processing task...", zap.String("id", accountID), zap.Bool("priority", isHighPriority))

		// Use a fresh context for each task
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)

		var err error
		if isHighPriority && strings.HasPrefix(accountID, "guest_relearn:") {
			platform := strings.TrimPrefix(accountID, "guest_relearn:")
			c.log.Info("🔄 Bot Worker (HIGH): Task is GUEST RE-LEARNING", zap.String("platform", platform))
			// Progressive retry loop
			for {
				pc, ok := c.multiGuestMgr.GetConfig(platform)
				if !ok {
					break
				}

				// Check if already valid
				if pc.IsValid && !pc.Disabled {
					c.log.Info("✅ Bot Worker: Platform is already valid, skipping task", zap.String("platform", platform))
					break
				}

				now := time.Now().Unix()
				if now < pc.NextAttemptAt {
					waitSec := pc.NextAttemptAt - now
					c.log.Info("⏳ Bot Worker: Throttling guest re-learning", 
						zap.String("platform", platform), 
						zap.Int64("wait_remaining_seconds", waitSec))
					
					// Nếu đang trong hàng đợi ưu tiên cao, ta có thể đợi một chút thay vì thoát hẳn
					if waitSec < 30 {
						time.Sleep(time.Duration(waitSec) * time.Second)
					} else {
						break // Quay lại sau khi task này được kích hoạt lại hoặc task khác trong hàng đợi được xử lý
					}
				}

				c.log.Info("🔄 Guest Re-learning Attempt", zap.String("platform", platform), zap.Int("fail_count", pc.FailCount))

				attemptCtx, attemptCancel := context.WithTimeout(ctx, 180*time.Second)
				var lErr error
				if platform == "gemini" {
					var gCfg *PlatformConfig
					gCfg, lErr = c.LearnGuestStructure(attemptCtx)
					if lErr == nil && gCfg != nil {
						c.multiGuestMgr.SaveConfig(*gCfg)
					}
				} else {
					lErr = c.discoverySvc.LearnNewPlatform(attemptCtx, pc.BaseURL, platform)
				}
				attemptCancel()

				if lErr == nil {
					c.log.Info("✅ Bot Worker: Guest re-learning SUCCESSFUL", zap.String("platform", platform))
					c.multiGuestMgr.ResetWaitInterval(platform)
					break
				}

				// Xử lý lỗi
				c.log.Warn("⏳ Bot Worker: Guest re-learning failed", zap.String("platform", platform), zap.Error(lErr))
				
				// Invalidate sẽ tăng fail_count và set NextAttemptAt
				c.multiGuestMgr.Invalidate(platform)
				
				// Sau khi thất bại 1 lần (thử hết API keys trong LearnGuestStructure/LearnNewPlatform), 
				// ta thoát ra để nhường chỗ cho các task khác, task này sẽ được Trigger lại sau.
				break 
			}
		} else {
			// Normal account bot clearance
			c.mu.RLock()
			acc, accExists := c.accountsMap[accountID]
			hasEmptyCookie := accExists && acc.Secure1PSID == ""
			c.mu.RUnlock()

			if hasEmptyCookie {
				c.log.Warn("⚠️ Bot Worker: Account has empty cookie. Triggering Guest re-learning instead.", zap.String("id", accountID))
				c.TriggerGuestLearning("gemini")
				err = nil
			} else {
				err = c.ClearBot(ctx, accountID)
			}
		}
		cancel()

		if err != nil {
			c.log.Error("❌ Bot Worker: Task FAILED", zap.String("id", accountID), zap.Error(err))
		} else {
			c.log.Info("✨ Bot Worker: Task SUCCESS", zap.String("id", accountID))
		}

		// Remove from pending map
		c.botPendingMap.Delete(accountID)

		// Recovery Delay: Only for non-critical tasks to protect 1GB RAM VPS
		if !isHighPriority {
			time.Sleep(5 * time.Second)
		} else {
			time.Sleep(1 * time.Second)
		}
	}
}

func (c *Client) SetGuestDisabled(platform string, disabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.multiGuestMgr.mu.Lock()
	defer c.multiGuestMgr.mu.Unlock()

	if pc, ok := c.multiGuestMgr.Configs[platform]; ok {
		pc.Disabled = disabled
		c.multiGuestMgr.Save()
		c.log.Info("🔄 Guest platform status updated", zap.String("platform", platform), zap.Bool("disabled", disabled))
	}
}

func (c *Client) HandleGuestError(id string, err error) {
	if !strings.HasPrefix(id, "guest-") {
		return
	}
	platform := strings.TrimPrefix(id, "guest-")
	c.log.Warn("⚠️ Guest worker request FAILED. Queuing re-learn (no disable).", zap.String("platform", platform), zap.Error(err))

	// IMPORTANT: Do NOT call Invalidate() here.
	// Invalidate increases fail_count and can permanently disable the guest after 5 request failures.
	// Guest errors during REQUEST handling are transient (bot flag, parse error, etc.)
	// and should only trigger re-learning, NOT counting against the platform's health.
	// Disable only happens when re-learning itself fails 5 times in a row (inside startBotWorker).
	c.TriggerGuestLearning(platform)
}

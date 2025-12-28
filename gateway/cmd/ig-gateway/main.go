package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	stdlog "log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/anthropics/instagram-dms-mcp/gateway/pkg/messagix"
	"github.com/anthropics/instagram-dms-mcp/gateway/pkg/messagix/cookies"
	"github.com/anthropics/instagram-dms-mcp/gateway/pkg/messagix/socket"
	"github.com/anthropics/instagram-dms-mcp/gateway/pkg/messagix/table"
	"github.com/anthropics/instagram-dms-mcp/gateway/pkg/messagix/types"
)

// abs returns the absolute value of an int64
func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// IGMessageEvent is a minimal representation of an incoming Instagram DM
// message, suitable for handing off to external consumers (e.g. discord-puppet).
type IGAttachment struct {
	Type           string `json:"type"`
	URL            string `json:"url,omitempty"`
	Filename       string `json:"filename,omitempty"`
	MediaID        string `json:"media_id,omitempty"`
	PreviewURL     string `json:"preview_url,omitempty"`
	MimeType       string `json:"mime_type,omitempty"`
	AuthorUsername string `json:"author_username,omitempty"` // For posts/reels, the username from TitleText
	ActionURL      string `json:"action_url,omitempty"`      // URL to the post (contains username)
}

type IGMessageEvent struct {
	ThreadID    string         `json:"thread_id"`
	MessageID   string         `json:"message_id"`
	SenderID    string         `json:"sender_id"`
	Text        string         `json:"text"`
	Timestamp   int64          `json:"timestamp_ms"`
	Attachments []IGAttachment `json:"attachments,omitempty"`
}

// eventQueue is a simple in-memory FIFO queue for IGMessageEvent.
type eventQueue struct {
	mu     sync.Mutex
	events []IGMessageEvent
}

func newEventQueue() *eventQueue {
	return &eventQueue{events: make([]IGMessageEvent, 0)}
}

func (q *eventQueue) add(evt IGMessageEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.events = append(q.events, evt)
}

// take removes and returns up to max events from the front of the queue.
func (q *eventQueue) take(max int) []IGMessageEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	if max <= 0 || len(q.events) == 0 {
		return nil
	}
	if max > len(q.events) {
		max = len(q.events)
	}
	out := make([]IGMessageEvent, max)
	copy(out, q.events[:max])
	q.events = q.events[max:]
	return out
}

type contactInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Username      string `json:"username"`
	ProfilePicURL string `json:"profile_pic_url,omitempty"`
}

// gatewayState is shared between the HTTP handlers and the messagix client.
type gatewayState struct {
	userID     string
	client     *messagix.Client
	queue      *eventQueue
	contacts   map[string]contactInfo
	contactsMu sync.RWMutex
	// selfIDs stores all IDs that represent this user (FBID, IUID, etc.)
	selfIDs   map[string]struct{}
	selfIDsMu sync.RWMutex
	// primaryFBID is the main Facebook ID for this user
	primaryFBID int64
	// initialHistory stores messages from the initial inbox load, keyed by thread ID
	initialHistory   map[int64][]IGMessageEvent
	initialHistoryMu sync.RWMutex
	// historyFetched tracks which threads we've already fetched full history for
	historyFetched   map[int64]bool
	historyFetchedMu sync.RWMutex
	// historyExhausted tracks which threads have no more messages on Instagram
	historyExhausted   map[int64]bool
	historyExhaustedMu sync.RWMutex
	// backfillWaiters allows waiting for history fetch responses
	backfillWaiters   map[int64]chan *table.UpsertMessages
	backfillWaitersMu sync.Mutex
}

func (s *gatewayState) updateContact(id int64, name, username, profilePicURL string) {
	if id == 0 {
		return
	}
	s.contactsMu.Lock()
	defer s.contactsMu.Unlock()
	sid := fmt.Sprintf("%d", id)
	// Prefer existing entries if values are missing in update
	if existing, ok := s.contacts[sid]; ok {
		if name == "" {
			name = existing.Name
		}
		if username == "" {
			username = existing.Username
		}
		if profilePicURL == "" {
			profilePicURL = existing.ProfilePicURL
		}
	}
	s.contacts[sid] = contactInfo{
		ID:            sid,
		Name:          name,
		Username:      username,
		ProfilePicURL: profilePicURL,
	}
}

func main() {
	// Basic CLI flags for configuration; can also be overridden by env vars.
	var (
		addr       = flag.String("addr", getenvDefault("IG_GATEWAY_ADDR", "127.0.0.1:29391"), "HTTP listen address")
		cookieFile = flag.String("cookies", "", "Path to JSON file with Instagram cookies")
	)
	flag.Parse()

	cookiePath := *cookieFile
	if cookiePath == "" {
		// Fall back to IG_COOKIES_FILE, if set.
		cookiePath = os.Getenv("IG_COOKIES_FILE")
	}
	if cookiePath == "" {
		// Default location: ~/.instagram-dms-mcp/cookies.json
		homeDir, err := os.UserHomeDir()
		if err == nil && homeDir != "" {
			candidate := filepath.Join(homeDir, ".instagram-dms-mcp", "cookies.json")
			if _, err := os.Stat(candidate); err == nil {
				cookiePath = candidate
			}
		}
	}
	if cookiePath == "" {
		stdlog.Fatalf("No cookies file found. Set IG_COOKIES_FILE or create ~/.instagram-dms-mcp/cookies.json")
	}

	// Configure zerolog to log to stderr in a simple format.
	// Set global level to Error to suppress noisy Warn logs from the messagix library
	// (e.g., "Unknown dependency", "Skipping dependency") which are harmless but spammy.
	// Our gateway logger is configured separately with InfoLevel.
	zerolog.TimeFieldFormat = time.RFC3339
	zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Create our own logger at Info level (not affected by global ErrorLevel)
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).
		With().
		Timestamp().
		Str("component", "ig-gateway").
		Str("cookie_path", cookiePath).
		Logger().
		Level(zerolog.InfoLevel)
	logger.Info().Msg("Using Instagram cookies file for gateway")

	cookiesObj, err := loadCookies(cookiePath)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load Instagram cookies")
	}
	if !cookiesObj.IsLoggedIn() {
		log.Fatal().Msg("Instagram cookies do not appear to contain a valid session (missing sessionid)")
	}

	client := messagix.NewClient(cookiesObj, logger, &messagix.Config{MayConnectToDGW: true})

	q := newEventQueue()


	// Initial inbox load (similar to MetaClient.connectWithTable).
	user, tbl, err := client.LoadMessagesPage(ctx)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to load Instagram messages page")
	}
	
	// Use the SAME logic as Beeper's ExtractFBID to find our true FBID.
	// This is critical: Instagram uses different IDs in different contexts.
	// The FBID from PolarisViewer may NOT match the sender_id on our own messages!
	// We need to find our IsSelf contact entry in the table.
	selfIDs := make(map[string]struct{})
	var primaryFBID int64
	
	// Step 1: Look for IsSelf contact in the initial table (this is what Beeper does!)
	if tbl != nil {
		for _, row := range tbl.LSVerifyContactRowExists {
			if row.IsSelf && row.ContactId != 0 {
				if primaryFBID != 0 && primaryFBID != row.ContactId {
					logger.Warn().
						Int64("prev_fbid", primaryFBID).
						Int64("new_fbid", row.ContactId).
						Msg("Got multiple FBIDs for self in table")
				}
				primaryFBID = row.ContactId
				selfIDs[fmt.Sprintf("%d", row.ContactId)] = struct{}{}
				logger.Info().Int64("fbid", row.ContactId).Msg("Found own FBID from IsSelf contact row")
			}
		}
	}
	
	// Step 2: Fallback to PolarisViewer data if no IsSelf contact found
	if primaryFBID == 0 {
		primaryFBID = user.GetFBID()
		logger.Debug().
			Int64("fbid", primaryFBID).
			Msg("IsSelf contact not found, falling back to PolarisViewer.GetFBID()")
	}
	
	// Also collect the IUID (Instagram User ID) which may differ from FBID.
	// The IUID is available via PolarisViewer.GetUserId() but not on the UserInfo interface,
	// so we type-assert if possible.
	var iuid string
	if pv, ok := user.(*types.PolarisViewer); ok {
		iuid = pv.GetUserId()
	}
	if iuid != "" {
		selfIDs[iuid] = struct{}{}
	}
	if primaryFBID != 0 {
		selfIDs[fmt.Sprintf("%d", primaryFBID)] = struct{}{}
	}
	
	userID := fmt.Sprintf("%d", primaryFBID)
	if userID == "0" {
		logger.Warn().Str("user_name", user.GetName()).Msg("Detected user ID 0 - self-reply protection may fail!")
	}
	
	logger.Info().
		Str("user_name", user.GetName()).
		Str("user_id", userID).
		Str("iuid", iuid).
		Int("self_id_count", len(selfIDs)).
		Msg("Loaded Instagram inbox and identified self")

	state := &gatewayState{
		userID:           userID,
		client:           client,
		queue:            q,
		contacts:         make(map[string]contactInfo),
		selfIDs:          selfIDs,
		primaryFBID:      primaryFBID,
		initialHistory:   make(map[int64][]IGMessageEvent),
		historyFetched:   make(map[int64]bool),
		historyExhausted: make(map[int64]bool),
		backfillWaiters:  make(map[int64]chan *table.UpsertMessages),
	}

	// Step 3: Also try to resolve via GetContactsFullTask for any additional ID mappings
	resolveID := primaryFBID
	if resolveID == 0 && iuid != "" {
		resolveID, _ = strconv.ParseInt(iuid, 10, 64)
	}

	if resolveID != 0 {
		go func() {
			logger.Info().Int64("resolve_id", resolveID).Msg("Attempting to resolve alternate self IDs via contacts fetch")
			resp, err := client.ExecuteTasks(ctx, &socket.GetContactsFullTask{
				ContactID: resolveID,
			})
			if err != nil {
				logger.Warn().Err(err).Msg("Failed to fetch self contact info")
				return
			}
			for _, info := range resp.LSDeleteThenInsertIGContactInfo {
				// If this contact info matches one of our known IDs, add the others
				igIDStr := fmt.Sprintf("%d", info.IgId)
				contactIDStr := fmt.Sprintf("%d", info.ContactId)
				
				state.selfIDsMu.RLock()
				_, hasIG := state.selfIDs[igIDStr]
				_, hasContact := state.selfIDs[contactIDStr]
				state.selfIDsMu.RUnlock()
				
				if hasIG || hasContact {
					state.selfIDsMu.Lock()
					state.selfIDs[igIDStr] = struct{}{}
					state.selfIDs[contactIDStr] = struct{}{}
					state.selfIDsMu.Unlock()
					logger.Info().
						Str("ig_id", igIDStr).
						Str("contact_id", contactIDStr).
						Msg("Resolved additional self ID mapping")
				}
			}
		}()
	}

	// Wire messagix event handler to push new messages into the queue.
	client.SetEventHandler(func(evCtx context.Context, evt any) {
		onMessagixEvent(logger, state, evCtx, evt)
	})

	// Bootstrap contacts and initial message history from the inbox table.
	if tbl != nil {
		initialHistory := bootstrapFromTable(logger, state, tbl)
		state.initialHistoryMu.Lock()
		state.initialHistory = initialHistory
		state.initialHistoryMu.Unlock()
	}

	// Start HTTP server.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		
		// Convert set to list
		state.selfIDsMu.RLock()
		idList := make([]string, 0, len(state.selfIDs))
		for id := range state.selfIDs {
			idList = append(idList, id)
		}
		state.selfIDsMu.RUnlock()
		
		// Try to get profile pic from contacts cache using any of our self IDs
		var profilePicURL string
		state.contactsMu.RLock()
		for _, id := range idList {
			if contact, ok := state.contacts[id]; ok && contact.ProfilePicURL != "" {
				profilePicURL = contact.ProfilePicURL
				break
			}
		}
		state.contactsMu.RUnlock()
		
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":          "ok",
			"user_id":         state.userID, // primary FBID
			"username":        user.GetUsername(),
			"ids":             idList, // All known self IDs
			"profile_pic_url": profilePicURL,
		})
	})
	mux.HandleFunc("/poll", func(w http.ResponseWriter, r *http.Request) {
		max := 50
		if m := r.URL.Query().Get("max"); m != "" {
			if v, err := strconv.Atoi(m); err == nil && v > 0 {
				max = v
			}
		}
		events := state.queue.take(max)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"events": events,
		})
	})
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ThreadID string `json:"thread_id"`
			Text     string `json:"text"`
			ReplyTo  string `json:"reply_to"` // Optional: message ID to reply to
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid JSON"))
			return
		}
		if req.ThreadID == "" || req.Text == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("thread_id and text are required"))
			return
		}
		threadKey, err := strconv.ParseInt(req.ThreadID, 10, 64)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid thread_id"))
			return
		}
		if err := sendTextMessage(ctx, state.client, threadKey, req.Text, req.ReplyTo); err != nil {
			logger.Error().Err(err).Int64("thread_key", threadKey).Msg("failed to send message")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("send failed"))
			return
		}

		// Add the sent message to our local cache so it appears in /history
		sentMsg := IGMessageEvent{
			ThreadID:   strconv.FormatInt(threadKey, 10),
			SenderID:   strconv.FormatInt(state.primaryFBID, 10),
			Text:       req.Text,
			Timestamp:  time.Now().UnixMilli(),
			MessageID:  fmt.Sprintf("sent-%d", time.Now().UnixNano()),
		}
		state.initialHistoryMu.Lock()
		state.initialHistory[threadKey] = append(state.initialHistory[threadKey], sentMsg)
		state.initialHistoryMu.Unlock()
		logger.Info().Int64("thread_id", threadKey).Msg("Added sent message to history cache")

		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/seen", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ThreadID string `json:"thread_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid JSON"))
			return
		}
		if req.ThreadID == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("thread_id is required"))
			return
		}
		threadKey, err := strconv.ParseInt(req.ThreadID, 10, 64)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid thread_id"))
			return
		}
		if state.client == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("client not connected"))
			return
		}
		// Mark the thread as read up to "now". This mirrors the ThreadMarkRead
		// task used in the main bridge, but without needing to look up the exact
		// message watermark.
		ts := time.Now().UnixMilli()
		_, err = state.client.ExecuteTasks(ctx, &socket.ThreadMarkReadTask{
			ThreadId:            threadKey,
			LastReadWatermarkTs: ts,
			SyncGroup:           1,
		})
		if err != nil {
			logger.Error().Err(err).Int64("thread_key", threadKey).Msg("failed to mark thread as read")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("seen failed"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		state.contactsMu.RLock()
		info, ok := state.contacts[id]
		state.contactsMu.RUnlock()

		if !ok {
			// If not found, return 404. We don't support on-demand fetching yet
			// to stay safe, relying on the table sync.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	})

	// /threads returns all threads from the initial inbox load with participant info
	mux.HandleFunc("/threads", func(w http.ResponseWriter, r *http.Request) {
		state.initialHistoryMu.RLock()
		defer state.initialHistoryMu.RUnlock()
		
		type ThreadInfo struct {
			ThreadID            string `json:"thread_id"`
			ParticipantID       string `json:"participant_id,omitempty"`
			ParticipantName     string `json:"participant_name,omitempty"`
			ParticipantUsername string `json:"participant_username,omitempty"`
			ProfilePicURL       string `json:"profile_pic_url,omitempty"`
			LastMessageTime     int64  `json:"last_message_time,omitempty"`
			LastMessagePreview  string `json:"last_message_preview,omitempty"`
			MessageCount        int    `json:"message_count"`
		}
		
		threads := make([]ThreadInfo, 0, len(state.initialHistory))
		state.selfIDsMu.RLock()
		state.contactsMu.RLock()
		
		for threadID, messages := range state.initialHistory {
			info := ThreadInfo{
				ThreadID:     strconv.FormatInt(threadID, 10),
				MessageCount: len(messages),
			}
			
			// For 1:1 DMs, the thread_id IS the other user's ID
			// Try to look up contact info using thread_id as user_id
			threadIDStr := strconv.FormatInt(threadID, 10)
			if contact, ok := state.contacts[threadIDStr]; ok {
				info.ParticipantID = threadIDStr
				info.ParticipantName = contact.Name
				info.ParticipantUsername = contact.Username
				info.ProfilePicURL = contact.ProfilePicURL
			}
			
			// Find last message and participant from messages if not found above
			for i := len(messages) - 1; i >= 0; i-- {
				msg := messages[i]
				// Get last message time and preview
				if info.LastMessageTime == 0 {
					info.LastMessageTime = msg.Timestamp
					if len(msg.Text) > 100 {
						info.LastMessagePreview = msg.Text[:100]
					} else {
						info.LastMessagePreview = msg.Text
					}
				}
				// Find participant (non-self sender) if not already found
				if info.ParticipantUsername == "" {
					if _, isSelf := state.selfIDs[msg.SenderID]; !isSelf {
						info.ParticipantID = msg.SenderID
						if contact, ok := state.contacts[msg.SenderID]; ok {
							info.ParticipantName = contact.Name
							info.ParticipantUsername = contact.Username
							info.ProfilePicURL = contact.ProfilePicURL
						}
					}
				}
				// Stop if we have everything
				if info.LastMessageTime != 0 && info.ParticipantUsername != "" {
					break
				}
			}
			
			threads = append(threads, info)
		}
		
		state.contactsMu.RUnlock()
		state.selfIDsMu.RUnlock()
		
		// Sort threads by LastMessageTime descending (most recent first)
		for i := 0; i < len(threads)-1; i++ {
			for j := i + 1; j < len(threads); j++ {
				if threads[i].LastMessageTime < threads[j].LastMessageTime {
					threads[i], threads[j] = threads[j], threads[i]
				}
			}
		}
		
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"threads": threads,
			"count":   len(threads),
		})
	})

	// /typing sends a typing indicator to Instagram for a thread.
	mux.HandleFunc("/typing", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ThreadID string `json:"thread_id"`
			Typing   bool   `json:"typing"` // true = start typing, false = stop
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid JSON"))
			return
		}
		if req.ThreadID == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("thread_id is required"))
			return
		}
		threadKey, err := strconv.ParseInt(req.ThreadID, 10, 64)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid thread_id"))
			return
		}
		if state.client == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("client not connected"))
			return
		}
		
		isTyping := int64(0)
		if req.Typing {
			isTyping = 1
		}
		
		// Use UpdatePresenceTask like Beeper does
		err = state.client.ExecuteStatelessTask(ctx, &socket.UpdatePresenceTask{
			ThreadKey:     threadKey,
			IsGroupThread: 0, // DMs are not group threads
			IsTyping:      isTyping,
			Attribution:   0,
			SyncGroup:     1,
			ThreadType:    1, // ONE_TO_ONE for DMs
		})
		if err != nil {
			logger.Warn().Err(err).Int64("thread_key", threadKey).Msg("failed to send typing indicator")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("typing failed"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// /history returns message history for a thread.
	// On first access for a thread, it fetches more history from Instagram (like opening a chat).
	// Subsequent calls return the cached history.
	mux.HandleFunc("/history", func(w http.ResponseWriter, r *http.Request) {
		threadIDStr := r.URL.Query().Get("thread_id")
		if threadIDStr == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("thread_id is required"))
			return
		}
		threadID, err := strconv.ParseInt(threadIDStr, 10, 64)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid thread_id"))
			return
		}

		// Limit how many messages to return (default 30, max 100)
		limit := 30
		if l := r.URL.Query().Get("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil && v > 0 {
				limit = v
			}
		}
		if limit > 100 {
			limit = 100
		}

		// Check if refresh is requested (forces fresh fetch from Instagram)
		forceRefresh := r.URL.Query().Get("refresh") == "true"

		// Check if we've already fetched full history for this thread
		state.historyFetchedMu.RLock()
		alreadyFetched := state.historyFetched[threadID]
		state.historyFetchedMu.RUnlock()

		if (!alreadyFetched || forceRefresh) && state.client != nil {
			// First time accessing this thread or refresh requested - fetch from Instagram
			state.historyFetchedMu.Lock()
			// Double-check after acquiring write lock (skip if already fetched, unless refresh forced)
			if !state.historyFetched[threadID] || forceRefresh {
				state.historyFetched[threadID] = true
				state.historyFetchedMu.Unlock()

				// Get existing messages to find reference point for pagination
				state.initialHistoryMu.RLock()
				existingMsgs := state.initialHistory[threadID]
				state.initialHistoryMu.RUnlock()

				var referenceTimestamp int64
				var referenceMessageID string
				if forceRefresh {
					// For refresh, use current time to fetch recent messages
					referenceTimestamp = time.Now().UnixMilli()
					referenceMessageID = ""
				} else if len(existingMsgs) > 0 {
					// Find the oldest message for backfill
					referenceTimestamp = existingMsgs[0].Timestamp
					referenceMessageID = existingMsgs[0].MessageID
					for _, m := range existingMsgs {
						if m.Timestamp < referenceTimestamp {
							referenceTimestamp = m.Timestamp
							referenceMessageID = m.MessageID
						}
					}
				} else {
					// No existing messages - use current time to fetch recent history
					referenceTimestamp = time.Now().UnixMilli()
				}

				// Create a channel to receive the backfill response
				waiter := make(chan *table.UpsertMessages, 1)
				state.backfillWaitersMu.Lock()
				state.backfillWaiters[threadID] = waiter
				state.backfillWaitersMu.Unlock()

				logger.Info().
					Int64("thread_id", threadID).
					Int64("reference_ts", referenceTimestamp).
					Str("reference_msg_id", referenceMessageID).
					Bool("refresh", forceRefresh).
					Msg("Fetching history for thread")

				// Request more history
				_, fetchErr := state.client.ExecuteTasks(ctx, &socket.FetchMessagesTask{
					ThreadKey:            threadID,
					Direction:            0,
					ReferenceTimestampMs: referenceTimestamp,
					ReferenceMessageId:   referenceMessageID,
					SyncGroup:            1,
					Cursor:               state.client.GetCursor(1),
				})

				if fetchErr != nil {
					logger.Warn().Err(fetchErr).Int64("thread_id", threadID).Msg("Failed to request more history")
				} else {
					// Wait for response with timeout
					select {
					case upsert := <-waiter:
						if upsert != nil {
							// Process the fetched messages
							newMsgs := make([]IGMessageEvent, 0, len(upsert.Messages))
							for _, msg := range upsert.Messages {
								if msg == nil || msg.MessageId == "" {
									continue
								}
								attachments := extractAttachments(msg)
								// Skip only if truly empty (no text AND no attachments)
								if msg.Text == "" && len(attachments) == 0 {
									continue
								}
								newMsgs = append(newMsgs, IGMessageEvent{
									ThreadID:    fmt.Sprintf("%d", msg.ThreadKey),
									MessageID:   msg.MessageId,
									SenderID:    fmt.Sprintf("%d", msg.SenderId),
									Text:        msg.Text,
									Timestamp:   msg.TimestampMs,
									Attachments: attachments,
								})
							}

							if len(newMsgs) > 0 {
								// Merge with existing history
								state.initialHistoryMu.Lock()
								existing := state.initialHistory[threadID]
								
								// Create a map to dedupe by message ID
								msgMap := make(map[string]IGMessageEvent)
								for _, m := range existing {
									msgMap[m.MessageID] = m
								}
								for _, m := range newMsgs {
									msgMap[m.MessageID] = m
								}
								
								// Remove sent- placeholder messages that have matching real messages
								// (same sender, same text, timestamp within 5 seconds)
								for sentID, sentMsg := range msgMap {
									if len(sentID) > 5 && sentID[:5] == "sent-" {
										for realID, realMsg := range msgMap {
											if len(realID) > 4 && realID[:4] == "mid." &&
												sentMsg.SenderID == realMsg.SenderID &&
												sentMsg.Text == realMsg.Text &&
												abs(sentMsg.Timestamp-realMsg.Timestamp) < 5000 {
												delete(msgMap, sentID)
												break
											}
										}
									}
								}
								
								// Convert back to slice and sort by timestamp
								merged := make([]IGMessageEvent, 0, len(msgMap))
								for _, m := range msgMap {
									merged = append(merged, m)
								}
								// Sort ascending by timestamp
								for i := 0; i < len(merged)-1; i++ {
									for j := i + 1; j < len(merged); j++ {
										if merged[i].Timestamp > merged[j].Timestamp {
											merged[i], merged[j] = merged[j], merged[i]
										}
									}
								}
								state.initialHistory[threadID] = merged
								state.initialHistoryMu.Unlock()

								logger.Info().
									Int64("thread_id", threadID).
									Int("fetched", len(newMsgs)).
									Int("total", len(merged)).
									Msg("Merged fetched history")
							}
						}
					case <-time.After(5 * time.Second):
						logger.Warn().Int64("thread_id", threadID).Msg("Timeout waiting for history fetch response")
					}
				}

				// Cleanup waiter
				state.backfillWaitersMu.Lock()
				delete(state.backfillWaiters, threadID)
				state.backfillWaitersMu.Unlock()
			} else {
				state.historyFetchedMu.Unlock()
			}
		}

		// Support pagination with "before" timestamp
		beforeTs := int64(0)
		if b := r.URL.Query().Get("before"); b != "" {
			if v, err := strconv.ParseInt(b, 10, 64); err == nil {
				beforeTs = v
			}
		}

		// If paginating (before is set), try to fetch more history from Instagram
		if beforeTs > 0 && state.client != nil {
			// Get current oldest message
			state.initialHistoryMu.RLock()
			existingMsgs := state.initialHistory[threadID]
			state.initialHistoryMu.RUnlock()

			// Find oldest message timestamp
			var oldestExisting int64 = beforeTs
			var oldestMsgID string
			for _, m := range existingMsgs {
				if m.Timestamp < oldestExisting {
					oldestExisting = m.Timestamp
					oldestMsgID = m.MessageID
				}
			}

			// Only fetch if we're near the oldest cached message
			if beforeTs <= oldestExisting+1000 { // Within 1 second of oldest
				// Create a channel to receive the backfill response
				waiter := make(chan *table.UpsertMessages, 1)
				state.backfillWaitersMu.Lock()
				state.backfillWaiters[threadID] = waiter
				state.backfillWaitersMu.Unlock()

				logger.Info().
					Int64("thread_id", threadID).
					Int64("before_ts", beforeTs).
					Str("oldest_msg_id", oldestMsgID).
					Msg("Fetching more history for pagination")

				// Request more history
				_, fetchErr := state.client.ExecuteTasks(ctx, &socket.FetchMessagesTask{
					ThreadKey:            threadID,
					Direction:            0,
					ReferenceTimestampMs: oldestExisting,
					ReferenceMessageId:   oldestMsgID,
					SyncGroup:            1,
					Cursor:               state.client.GetCursor(1),
				})

				if fetchErr != nil {
					logger.Warn().Err(fetchErr).Int64("thread_id", threadID).Msg("Failed to request more history for pagination")
				} else {
					// Wait for response with timeout
					select {
					case upsert := <-waiter:
						if upsert != nil {
							// Process the fetched messages
							newMsgs := make([]IGMessageEvent, 0, len(upsert.Messages))
							for _, msg := range upsert.Messages {
								if msg == nil || msg.MessageId == "" {
									continue
								}
								attachments := extractAttachments(msg)
								if msg.Text == "" && len(attachments) == 0 {
									continue
								}
								newMsgs = append(newMsgs, IGMessageEvent{
									ThreadID:    fmt.Sprintf("%d", msg.ThreadKey),
									MessageID:   msg.MessageId,
									SenderID:    fmt.Sprintf("%d", msg.SenderId),
									Text:        msg.Text,
									Timestamp:   msg.TimestampMs,
									Attachments: attachments,
								})
							}

							if len(newMsgs) > 0 {
								// Merge with existing history
								state.initialHistoryMu.Lock()
								existing := state.initialHistory[threadID]

								// Create a map to dedupe by message ID
								msgMap := make(map[string]IGMessageEvent)
								for _, m := range existing {
									msgMap[m.MessageID] = m
								}
								for _, m := range newMsgs {
									msgMap[m.MessageID] = m
								}

								// Remove sent- placeholder messages that have matching real messages
								// (same sender, same text, timestamp within 5 seconds)
								for sentID, sentMsg := range msgMap {
									if len(sentID) > 5 && sentID[:5] == "sent-" {
										for realID, realMsg := range msgMap {
											if len(realID) > 4 && realID[:4] == "mid." &&
												sentMsg.SenderID == realMsg.SenderID &&
												sentMsg.Text == realMsg.Text &&
												abs(sentMsg.Timestamp-realMsg.Timestamp) < 5000 {
												delete(msgMap, sentID)
												break
											}
										}
									}
								}

								// Convert back to slice and sort by timestamp
								merged := make([]IGMessageEvent, 0, len(msgMap))
								for _, m := range msgMap {
									merged = append(merged, m)
								}
								// Sort ascending by timestamp
								for i := 0; i < len(merged)-1; i++ {
									for j := i + 1; j < len(merged); j++ {
										if merged[i].Timestamp > merged[j].Timestamp {
											merged[i], merged[j] = merged[j], merged[i]
										}
									}
								}
								state.initialHistory[threadID] = merged
								state.initialHistoryMu.Unlock()

								logger.Info().
									Int64("thread_id", threadID).
									Int("fetched", len(newMsgs)).
									Int("total", len(merged)).
									Msg("Merged paginated history")
							} else {
								// No new messages fetched - mark thread as exhausted
								state.historyExhaustedMu.Lock()
								state.historyExhausted[threadID] = true
								state.historyExhaustedMu.Unlock()
								logger.Info().Int64("thread_id", threadID).Msg("No more messages from Instagram, marking history as exhausted")
							}
						} else {
							// Nil response - mark as exhausted
							state.historyExhaustedMu.Lock()
							state.historyExhausted[threadID] = true
							state.historyExhaustedMu.Unlock()
							logger.Info().Int64("thread_id", threadID).Msg("Nil response from Instagram, marking history as exhausted")
						}
					case <-time.After(5 * time.Second):
						logger.Warn().Int64("thread_id", threadID).Msg("Timeout waiting for pagination history")
					}
				}

				// Cleanup waiter
				state.backfillWaitersMu.Lock()
				delete(state.backfillWaiters, threadID)
				state.backfillWaitersMu.Unlock()
			}
		}

		// Return the cached history
		state.initialHistoryMu.RLock()
		msgs := state.initialHistory[threadID]
		state.initialHistoryMu.RUnlock()

		// Filter messages before the given timestamp
		var filtered []IGMessageEvent
		if beforeTs > 0 {
			for _, m := range msgs {
				if m.Timestamp < beforeTs {
					filtered = append(filtered, m)
				}
			}
		} else {
			filtered = msgs
		}

		// Return the most recent messages up to limit
		if len(filtered) > limit {
			filtered = filtered[len(filtered)-limit:]
		}

		// Get oldest timestamp for next pagination
		var oldestTs int64
		if len(filtered) > 0 {
			oldestTs = filtered[0].Timestamp
		}

		// Determine if there are more messages available
		// has_more is true if:
		// 1. We have more cached messages than the limit, OR
		// 2. We have messages AND haven't confirmed Instagram has no more
		state.historyExhaustedMu.RLock()
		exhausted := state.historyExhausted[threadID]
		state.historyExhaustedMu.RUnlock()

		hasMore := false
		if len(msgs) > limit {
			// More cached messages than requested
			hasMore = true
		} else if len(filtered) > 0 && !exhausted {
			// We have messages and haven't confirmed Instagram is exhausted
			// Allow the frontend to try fetching more
			hasMore = true
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"thread_id":  threadIDStr,
			"messages":   filtered,
			"has_more":   hasMore,
			"oldest_ts":  oldestTs,
		})
	})

	// /media sends an image or file to a thread.
	mux.HandleFunc("/media", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ThreadID string `json:"thread_id"`
			Filename string `json:"filename"`
			MimeType string `json:"mime_type"`
			Data     string `json:"data"` // Base64 encoded media data
			Caption  string `json:"caption,omitempty"`
			ReplyTo  string `json:"reply_to,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid JSON"))
			return
		}
		if req.ThreadID == "" || req.Data == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("thread_id and data are required"))
			return
		}
		threadKey, err := strconv.ParseInt(req.ThreadID, 10, 64)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid thread_id"))
			return
		}
		if state.client == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("client not connected"))
			return
		}

		// Decode base64 data
		mediaData, err := base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid base64 data"))
			return
		}

		// Default filename and mime type
		filename := req.Filename
		if filename == "" {
			filename = "image.jpg"
		}
		mimeType := req.MimeType
		if mimeType == "" {
			mimeType = "image/jpeg"
		}

		// Upload the media
		resp, err := state.client.SendMercuryUploadRequest(ctx, threadKey, &messagix.MercuryUploadMedia{
			Filename:  filename,
			MimeType:  mimeType,
			MediaData: mediaData,
		})
		if err != nil {
			logger.Error().Err(err).Int64("thread_key", threadKey).Msg("failed to upload media")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("media upload failed"))
			return
		}

		attachmentID := resp.Payload.RealMetadata.GetFbId()
		if attachmentID == 0 {
			logger.Error().Int64("thread_key", threadKey).Msg("no attachment ID received from upload")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("no attachment ID received"))
			return
		}

		// Send message with the attachment
		ts := time.Now().UnixMilli()
		task := &socket.SendMessageTask{
			ThreadId:          threadKey,
			Otid:              ts,
			Source:            table.MESSENGER_INBOX_IN_THREAD,
			InitiatingSource:  table.FACEBOOK_INBOX,
			SendType:          table.MEDIA,
			SyncGroup:         1,
			AttachmentFBIds:   []int64{attachmentID},
			Text:              req.Caption,
			SkipUrlPreviewGen: 1,
			MultiTabEnv:       0,
		}

		if req.ReplyTo != "" {
			task.ReplyMetaData = &socket.ReplyMetaData{
				ReplyMessageId:  req.ReplyTo,
				ReplySourceType: 1,
				ReplyType:       0,
			}
		}

		_, err = state.client.ExecuteTasks(ctx, task)
		if err != nil {
			logger.Error().Err(err).Int64("thread_key", threadKey).Msg("failed to send media message")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("send media message failed"))
			return
		}

		logger.Info().
			Int64("thread_key", threadKey).
			Int64("attachment_id", attachmentID).
			Str("filename", filename).
			Msg("Sent media message")
		w.WriteHeader(http.StatusNoContent)
	})

	// /like likes an Instagram post by media ID.
	mux.HandleFunc("/like", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			MediaID string `json:"media_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid JSON"))
			return
		}
		if req.MediaID == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("media_id is required"))
			return
		}
		if state.client == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("client not connected"))
			return
		}

		// Like the post using Instagram's GraphQL API
		err := likePost(ctx, state.client, req.MediaID)
		if err != nil {
			logger.Error().Err(err).Str("media_id", req.MediaID).Msg("failed to like post")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("like failed: " + err.Error()))
			return
		}
		logger.Info().Str("media_id", req.MediaID).Msg("Liked Instagram post")
		w.WriteHeader(http.StatusNoContent)
	})

	// /react sends a reaction to a message.
	mux.HandleFunc("/react", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ThreadID  string `json:"thread_id"`
			MessageID string `json:"message_id"`
			Emoji     string `json:"emoji"` // Unicode emoji, empty to remove reaction
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid JSON"))
			return
		}
		if req.ThreadID == "" || req.MessageID == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("thread_id and message_id are required"))
			return
		}
		threadKey, err := strconv.ParseInt(req.ThreadID, 10, 64)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid thread_id"))
			return
		}
		if state.client == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("client not connected"))
			return
		}

		// Use SendReactionTask to send the reaction
		_, err = state.client.ExecuteTasks(ctx, &socket.SendReactionTask{
			ThreadKey:       threadKey,
			MessageID:       req.MessageID,
			ActorID:         state.primaryFBID,
			Reaction:        req.Emoji, // Empty string removes reaction
			SendAttribution: table.MESSENGER_INBOX_IN_THREAD,
		})
		if err != nil {
			logger.Error().Err(err).
				Int64("thread_key", threadKey).
				Str("message_id", req.MessageID).
				Str("emoji", req.Emoji).
				Msg("failed to send reaction")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("reaction failed"))
			return
		}
		logger.Info().
			Int64("thread_key", threadKey).
			Str("message_id", req.MessageID).
			Str("emoji", req.Emoji).
			Msg("Sent reaction")
		w.WriteHeader(http.StatusNoContent)
	})

	// /dm sends a direct message to a user by their user ID (creates thread if needed).
	mux.HandleFunc("/dm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			UserID string `json:"user_id"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid JSON"))
			return
		}
		if req.UserID == "" || req.Text == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("user_id and text are required"))
			return
		}
		userID, err := strconv.ParseInt(req.UserID, 10, 64)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid user_id"))
			return
		}
		if state.client == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("client not connected"))
			return
		}

		// Send a DM to the user - Instagram will create the thread if it doesn't exist
		if err := sendDMToUser(ctx, state.client, userID, req.Text); err != nil {
			logger.Error().Err(err).Int64("user_id", userID).Msg("failed to send DM to user")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("dm failed: " + err.Error()))
			return
		}
		logger.Info().Int64("user_id", userID).Msg("Sent DM to user")
		w.WriteHeader(http.StatusNoContent)
	})

	// /dm_username sends a direct message to a user by their username (searches for user first).
	mux.HandleFunc("/dm_username", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Username string `json:"username"`
			Text     string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid JSON"))
			return
		}
		if req.Username == "" || req.Text == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("username and text are required"))
			return
		}
		if state.client == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("client not connected"))
			return
		}

		// Search for the user by username
		userID, err := searchUserByUsername(ctx, state.client, req.Username, logger)
		if err != nil {
			logger.Error().Err(err).Str("username", req.Username).Msg("failed to find user by username")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("user not found: " + err.Error()))
			return
		}

		// Send a DM to the user
		if err := sendDMToUser(ctx, state.client, userID, req.Text); err != nil {
			logger.Error().Err(err).Str("username", req.Username).Int64("user_id", userID).Msg("failed to send DM to user")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("dm failed: " + err.Error()))
			return
		}
		logger.Info().Str("username", req.Username).Int64("user_id", userID).Msg("Sent DM to user by username")
		
		// Return the thread_id (which is the user_id for 1:1 DMs)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"thread_id": strconv.FormatInt(userID, 10),
			"user_id":   userID,
		})
	})

	// /lookup_user looks up a user by username and returns their user_id (which is also the thread_id for 1:1 DMs)
	mux.HandleFunc("/lookup_user", func(w http.ResponseWriter, r *http.Request) {
		username := r.URL.Query().Get("username")
		if username == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("username query param is required"))
			return
		}
		if state.client == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("client not connected"))
			return
		}

		userID, err := searchUserByUsername(ctx, state.client, username, logger)
		if err != nil {
			logger.Debug().Err(err).Str("username", username).Msg("user not found")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("user not found"))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"username":  username,
			"user_id":   userID,
			"thread_id": strconv.FormatInt(userID, 10),
		})
	})

	server := &http.Server{
		Addr:    *addr,
		Handler: mux,
	}

	go func() {
		logger.Info().Str("addr", *addr).Msg("Starting IG gateway HTTP server")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal().Err(err).Msg("HTTP server error")
		}
	}()

	// Connect to the Instagram DM websocket and block until context is cancelled.
	if err := client.Connect(ctx); err != nil {
		logger.Fatal().Err(err).Msg("failed to connect to Instagram socket")
	}

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	logger.Info().Msg("IG gateway shut down")
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadCookies(path string) (*cookies.Cookies, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read cookies file: %w", err)
	}
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse cookies JSON: %w", err)
	}
	c := &cookies.Cookies{Platform: types.Instagram}
	c.UpdateValues(raw)
	return c, nil
}

// onMessagixEvent handles events from the messagix client and pushes new
// messages into the event queue.
func onMessagixEvent(logger zerolog.Logger, state *gatewayState, ctx context.Context, evt any) {
	switch ev := evt.(type) {
	case *messagix.Event_PublishResponse:
		if ev.Table == nil {
			return
		}
		// WrapMessages gives us both upserts and inserts. For IG DMs, new
		// messages may appear as LSUpsertMessage entries rather than pure
		// LSInsertMessage rows, so we need to consider both to avoid missing
		// events.
		upserts, inserts := ev.Table.WrapMessages()
		seen := make(map[string]struct{})
		count := 0

		// Only process messages from the last 30 seconds as "new" events.
		// This prevents old messages from initial sync/reconnect from being
		// treated as new DMs that need a response.
		nowMs := time.Now().UnixMilli()
		maxAgeMs := int64(30 * 1000) // 30 seconds

		addMsg := func(msg *table.WrappedMessage) {
			if msg == nil {
				return
			}
			if msg.MessageId == "" {
				return
			}
			if _, ok := seen[msg.MessageId]; ok {
				return
			}
			text := msg.Text

			attachments := extractAttachments(msg)
			
			// If no text and no attachments, skip
			if text == "" && len(attachments) == 0 {
				return
			}
			// Skip old messages - only queue messages from the last 30 seconds
			// This prevents responding to old DMs during sync/reconnect
			ageMs := nowMs - msg.TimestampMs
			if ageMs > maxAgeMs {
				logger.Debug().
					Str("message_id", msg.MessageId).
					Int64("age_seconds", ageMs/1000).
					Msg("Skipping old message from sync (not a new DM)")
				return
			}
			threadID := fmt.Sprintf("%d", msg.ThreadKey)
			e := IGMessageEvent{
				ThreadID:    threadID,
				MessageID:   msg.MessageId,
				SenderID:    fmt.Sprintf("%d", msg.SenderId),
				Text:        text,
				Timestamp:   msg.TimestampMs,
				Attachments: attachments,
			}
			state.queue.add(e)
			
			// Also add to initialHistory so /threads reflects the latest messages
			state.initialHistoryMu.Lock()
			existing := state.initialHistory[msg.ThreadKey]
			// Check if message already exists (by ID)
			found := false
			for _, m := range existing {
				if m.MessageID == msg.MessageId {
					found = true
					break
				}
			}
			if !found {
				state.initialHistory[msg.ThreadKey] = append(existing, e)
				// Keep sorted by timestamp
				msgs := state.initialHistory[msg.ThreadKey]
				for i := 0; i < len(msgs)-1; i++ {
					for j := i + 1; j < len(msgs); j++ {
						if msgs[i].Timestamp > msgs[j].Timestamp {
							msgs[i], msgs[j] = msgs[j], msgs[i]
						}
					}
				}
			}
			state.initialHistoryMu.Unlock()
			
			seen[msg.MessageId] = struct{}{}
			count++
		}

		// Check if any upserts are backfill responses (have multiple messages for a thread)
		for _, um := range upserts {
			if um == nil {
				continue
			}
			
			// Check if there's a waiter for this thread's backfill
			if len(um.Messages) > 0 {
				threadKey := um.Messages[0].ThreadKey
				state.backfillWaitersMu.Lock()
				waiter, hasWaiter := state.backfillWaiters[threadKey]
				state.backfillWaitersMu.Unlock()
				
				if hasWaiter {
					// This is a backfill response - send to waiter, don't enqueue as new events
					logger.Debug().
						Int64("thread_id", threadKey).
						Int("msg_count", len(um.Messages)).
						Msg("Received backfill response")
					select {
					case waiter <- um:
					default:
						// Waiter already received or closed
					}
					continue // Don't process these as new events
				}
			}
			
			// Normal new messages - enqueue them
			for _, msg := range um.Messages {
				addMsg(msg)
			}
		}
		for _, msg := range inserts {
			addMsg(msg)
		}
		if count > 0 {
			logger.Debug().
				Int("count", count).
				Msg("Enqueued IG DM events from PublishResponse")
		}
		
		// Also process contact updates
		for _, c := range ev.Table.LSDeleteThenInsertContact {
			state.updateContact(c.Id, c.Name, c.Username, c.GetAvatarURL())
		}
		for _, c := range ev.Table.LSVerifyContactRowExists {
			state.updateContact(c.ContactId, c.Name, c.SecondaryName, c.GetAvatarURL())
		}
	default:
		// Ignore other events for now.
	}
}

// extractAttachments collects all attachment types from a WrappedMessage, similar to Beeper's bridge.
func extractAttachments(msg *table.WrappedMessage) []IGAttachment {
	var atts []IGAttachment

	// BlobAttachments (common for images/videos)
	for _, blob := range msg.BlobAttachments {
		if blob == nil {
			continue
		}
		url := blob.PreviewUrlLarge
		if url == "" {
			url = blob.PreviewUrl
		}
		if url == "" {
			url = blob.PlayableUrl
		}
		att := IGAttachment{
			Type:       fmt.Sprintf("%d", blob.AttachmentType),
			URL:        url,
			Filename:   blob.Filename,
			PreviewURL: blob.PreviewUrl,
			MimeType:   blob.AttachmentMimeType,
		}
		if blob.AttachmentFbid != "" {
			att.MediaID = blob.AttachmentFbid
		}
		atts = append(atts, att)
	}

	// Attachments (legacy)
	for _, attObj := range msg.Attachments {
		if attObj == nil {
			continue
		}
		url := attObj.PreviewUrl
		if url == "" {
			url = attObj.ImageUrl
		}
		if url == "" {
			url = attObj.PlayableUrl
		}
		att := IGAttachment{
			Type:       fmt.Sprintf("%d", attObj.AttachmentType),
			URL:        url,
			Filename:   attObj.Filename,
			PreviewURL: attObj.PreviewUrl,
			MimeType:   attObj.AttachmentMimeType,
		}
		if attObj.AttachmentFbid != "" {
			att.MediaID = attObj.AttachmentFbid
		}
		atts = append(atts, att)
	}

	// XMAAttachments (rich media / posts)
	for _, xma := range msg.XMAAttachments {
		if xma == nil {
			continue
		}
		url := xma.PreviewUrl
		if url == "" {
			url = xma.ImageUrl
		}
		if url == "" {
			url = xma.PlayableUrl
		}
		att := IGAttachment{
			Type:       fmt.Sprintf("%d", xma.AttachmentType),
			URL:        url,
			Filename:   xma.Filename,
			PreviewURL: xma.PreviewUrl,
			MimeType:   xma.PlayableUrlMimeType,
		}
		if xma.AttachmentFbid != "" {
			att.MediaID = xma.AttachmentFbid
		}
		// Try to extract author username from ActionUrl (e.g., https://www.instagram.com/username/p/...)
		// or from TitleText/HeaderTitle which often contains the username
		att.AuthorUsername = xma.TitleText
		if att.AuthorUsername == "" {
			att.AuthorUsername = xma.HeaderTitle
		}
		att.ActionURL = xma.ActionUrl
		atts = append(atts, att)
	}

	// Stickers
	for _, st := range msg.Stickers {
		if st == nil {
			continue
		}
		att := IGAttachment{
			Type:     "sticker",
			URL:      st.ImageUrl,
			Filename: "",
			MimeType: st.ImageUrlMimeType,
		}
		atts = append(atts, att)
	}

	return atts
}

// bootstrapFromTable scans the initial table for contacts and pre-fills the cache.
// It also returns any existing messages from the table, organized by thread.
func bootstrapFromTable(logger zerolog.Logger, state *gatewayState, tbl *table.LSTable) map[int64][]IGMessageEvent {
	contactCount := 0
	for _, c := range tbl.LSDeleteThenInsertContact {
		state.updateContact(c.Id, c.Name, c.Username, c.GetAvatarURL())
		contactCount++
	}
	for _, c := range tbl.LSVerifyContactRowExists {
		state.updateContact(c.ContactId, c.Name, c.SecondaryName, c.GetAvatarURL())
		contactCount++
	}
	if contactCount > 0 {
		logger.Info().Int("count", contactCount).Msg("Loaded contacts from initial inbox table")
	}

	// Also extract existing messages from the table for history
	messagesByThread := make(map[int64][]IGMessageEvent)
	upserts, inserts := tbl.WrapMessages()
	
	addMsg := func(msg *table.WrappedMessage) {
		if msg == nil || msg.MessageId == "" {
			return
		}
		attachments := extractAttachments(msg)
		// Skip only if truly empty (no text AND no attachments)
		if msg.Text == "" && len(attachments) == 0 {
			return
		}
		e := IGMessageEvent{
			ThreadID:    fmt.Sprintf("%d", msg.ThreadKey),
			MessageID:   msg.MessageId,
			SenderID:    fmt.Sprintf("%d", msg.SenderId),
			Text:        msg.Text,
			Timestamp:   msg.TimestampMs,
			Attachments: attachments,
		}
		messagesByThread[msg.ThreadKey] = append(messagesByThread[msg.ThreadKey], e)
	}

	for _, um := range upserts {
		if um == nil {
			continue
		}
		for _, msg := range um.Messages {
			addMsg(msg)
		}
	}
	for _, msg := range inserts {
		addMsg(msg)
	}

	// Sort messages by timestamp within each thread
	for threadID := range messagesByThread {
		msgs := messagesByThread[threadID]
		// Sort ascending by timestamp
		for i := 0; i < len(msgs)-1; i++ {
			for j := i + 1; j < len(msgs); j++ {
				if msgs[i].Timestamp > msgs[j].Timestamp {
					msgs[i], msgs[j] = msgs[j], msgs[i]
				}
			}
		}
		messagesByThread[threadID] = msgs
	}

	msgCount := 0
	for _, msgs := range messagesByThread {
		msgCount += len(msgs)
	}
	if msgCount > 0 {
		logger.Info().Int("messages", msgCount).Int("threads", len(messagesByThread)).Msg("Loaded messages from initial inbox table")
	}

	return messagesByThread
}

// likePost likes an Instagram post using the GraphQL API.
// This uses the same mutation that the Instagram web app uses.
func likePost(ctx context.Context, client *messagix.Client, mediaID string) error {
	if client == nil {
		return fmt.Errorf("client is nil")
	}

	// Build the GraphQL request payload
	// This mirrors what Instagram web does when you click the like button
	variables := map[string]string{
		"media_id":         mediaID,
		"container_module": "feed_timeline",
	}
	variablesJSON, err := json.Marshal(variables)
	if err != nil {
		return fmt.Errorf("failed to marshal variables: %w", err)
	}

	// Build form data
	form := make(map[string]string)
	form["variables"] = string(variablesJSON)
	form["doc_id"] = "23951234354462179" // GraphQL mutation ID for usePolarisLikeMediaLikeMutation
	form["fb_api_req_friendly_name"] = "usePolarisLikeMediaLikeMutation"
	form["fb_api_caller_class"] = "RelayModern"
	form["server_timestamps"] = "true"

	// Make the request using the client's HTTP infrastructure
	resp, err := client.Instagram.LikePost(ctx, form)
	if err != nil {
		return fmt.Errorf("like request failed: %w", err)
	}

	// Check response
	if resp.Status != "ok" {
		return fmt.Errorf("like failed with status: %s", resp.Status)
	}

	return nil
}

// sendTextMessage uses a SendMessageTask via the messagix socket to send a
// plain text message into a thread. If replyToMessageID is non-empty, the
// message will be sent as a reply to that message.
func sendTextMessage(ctx context.Context, client *messagix.Client, threadKey int64, text string, replyToMessageID string) error {
	if client == nil {
		return fmt.Errorf("client is nil")
	}
	ts := time.Now().UnixMilli()
	// Minimal task fields copied from the bridge's ToMeta implementation.
	task := &socket.SendMessageTask{
		ThreadId:          threadKey,
		Otid:              ts,
		Source:            table.MESSENGER_INBOX_IN_THREAD,
		InitiatingSource:  table.FACEBOOK_INBOX,
		SendType:          table.TEXT,
		SyncGroup:         1,
		Text:              text,
		SkipUrlPreviewGen: 1,
		TextHasLinks:      0,
		MultiTabEnv:       0,
	}

	// Add reply metadata if replying to a specific message
	if replyToMessageID != "" {
		task.ReplyMetaData = &socket.ReplyMetaData{
			ReplyMessageId:  replyToMessageID,
			ReplySourceType: 1,
			ReplyType:       0,
		}
	}

	_, err := client.ExecuteTasks(ctx, task)
	return err
}

// sendDMToUser sends a direct message to a user by their user ID.
// Instagram will automatically create the thread if it doesn't exist.
// This uses the user's FBID as the thread key for 1:1 DMs.
func sendDMToUser(ctx context.Context, client *messagix.Client, userID int64, text string) error {
	if client == nil {
		return fmt.Errorf("client is nil")
	}
	
	// For Instagram 1:1 DMs, the thread key is the same as the user's FBID
	// Instagram will create the thread if it doesn't exist when we send a message
	ts := time.Now().UnixMilli()
	task := &socket.SendMessageTask{
		ThreadId:          userID, // Use user ID as thread key for 1:1 DMs
		Otid:              ts,
		Source:            table.MESSENGER_INBOX_IN_THREAD,
		InitiatingSource:  table.FACEBOOK_INBOX,
		SendType:          table.TEXT,
		SyncGroup:         1,
		Text:              text,
		SkipUrlPreviewGen: 1,
		TextHasLinks:      0,
		MultiTabEnv:       0,
	}

	_, err := client.ExecuteTasks(ctx, task)
	return err
}

// searchUserByUsername searches for a user by their Instagram username and returns their user ID.
func searchUserByUsername(ctx context.Context, client *messagix.Client, username string, logger zerolog.Logger) (int64, error) {
	if client == nil {
		return 0, fmt.Errorf("client is nil")
	}

	logger.Info().Str("username", username).Msg("Searching for user by username")

	// Use SearchUserTask to find the user - use the same types as Beeper's bridge
	task := &socket.SearchUserTask{
		Query: username,
		SupportedTypes: []table.SearchType{
			table.SearchTypeContact, table.SearchTypeGroup, table.SearchTypePage, table.SearchTypeNonContact,
			table.SearchTypeIGContactFollowing, table.SearchTypeIGContactNonFollowing,
			table.SearchTypeIGNonContactFollowing, table.SearchTypeIGNonContactNonFollowing,
		},
		SurfaceType: 15,
	}

	resp, err := client.ExecuteTasks(ctx, task)
	if err != nil {
		return 0, fmt.Errorf("search task failed: %w", err)
	}

	// The response is the LSTable directly
	if resp == nil {
		return 0, fmt.Errorf("no search results returned (nil response)")
	}

	logger.Info().
		Int("search_result_count", len(resp.LSInsertSearchResult)).
		Msg("Search response received")

	// Search results come in LSInsertSearchResult (this is what Beeper uses!)
	for i, result := range resp.LSInsertSearchResult {
		if result == nil {
			continue
		}
		fbid := result.GetFBID()
		resultUsername := result.GetUsername()
		resultName := result.GetName()
		
		logger.Debug().
			Int("index", i).
			Int64("fbid", fbid).
			Str("username", resultUsername).
			Str("name", resultName).
			Bool("can_message", result.CanViewerMessage).
			Msg("Found search result")
		
		// Check for username match
		if strings.EqualFold(resultUsername, username) && fbid != 0 {
			logger.Info().Int64("user_id", fbid).Str("username", resultUsername).Msg("Found exact username match")
			return fbid, nil
		}
	}

	// If no exact match, return the first messageable result
	for _, result := range resp.LSInsertSearchResult {
		if result != nil && result.CanViewerMessage && result.GetFBID() != 0 {
			fbid := result.GetFBID()
			logger.Info().Int64("user_id", fbid).Msg("Using first messageable result as fallback")
			return fbid, nil
		}
	}

	return 0, fmt.Errorf("user @%s not found in search results (%d results, none matched)", username, len(resp.LSInsertSearchResult))
}

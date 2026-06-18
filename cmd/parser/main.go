package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"parserTgChat/internal/app"
	msgparser "parserTgChat/internal/parser"
	filestorage "parserTgChat/internal/storage"
	tgclient "parserTgChat/internal/telegram"
)

func main() {
	if err := run(); err != nil {
		log.Printf("service error: %v", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	if err := godotenv.Load(); err != nil {
		log.Printf("load .env: %v", err)
	}

	ctx, cancel, err := commandContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	apiIDValue, err := requiredEnv("TELEGRAM_API_ID")
	if err != nil {
		return err
	}

	apiID, err := strconv.Atoi(apiIDValue)
	if err != nil {
		return fmt.Errorf("invalid TELEGRAM_API_ID: %w", err)
	}

	apiHash, err := requiredEnv("TELEGRAM_API_HASH")
	if err != nil {
		return err
	}
	phone, err := requiredEnv("TELEGRAM_PHONE")
	if err != nil {
		return err
	}

	sessionPath := envOrDefault("TELEGRAM_SESSION_PATH", "data/session.json")
	authMethod := envOrDefault("TELEGRAM_AUTH_METHOD", "code")

	client := tgclient.NewClient(apiID, apiHash, sessionPath)

	runApp := func(ctx context.Context, client *tgclient.Client) error {
		fmt.Println("telegram client connected")
		return runCommand(ctx, client)
	}

	var runErr error
	switch authMethod {
	case "qr":
		runErr = client.RunQR(ctx, runApp)
	case "code":
		runErr = client.Run(ctx, phone, runApp)
	default:
		return fmt.Errorf("unsupported TELEGRAM_AUTH_METHOD %q, use code or qr", authMethod)
	}

	return runErr
}

func requiredEnv(key string) (string, error) {
	value := os.Getenv(key)
	if value == "" {
		return "", fmt.Errorf("%s is required", key)
	}

	return value, nil
}

func envOrDefault(key string, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	return value
}

func commandContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	defaultTimeout := "60s"
	if len(os.Args) > 1 && os.Args[1] == "watch" {
		defaultTimeout = "0"
	}

	timeoutValue := envOrDefault("TELEGRAM_TIMEOUT", defaultTimeout)
	if timeoutValue == "0" {
		return parent, func() {}, nil
	}

	timeout, err := time.ParseDuration(timeoutValue)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid TELEGRAM_TIMEOUT: %w", err)
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	return ctx, cancel, nil
}

func runCommand(ctx context.Context, client *tgclient.Client) error {
	if len(os.Args) < 2 {
		return nil
	}

	switch os.Args[1] {
	case "sync-chats":
		return syncChats(ctx, client, os.Args[2:])
	case "list-chats":
		return listChats(ctx, client)
	case "history":
		return showHistory(ctx, client, os.Args[2:])
	case "parse":
		return parseChats(ctx, client, os.Args[2:])
	case "reparse":
		return reparseChats(ctx, client, os.Args[2:])
	case "reset-checkpoint":
		return resetCheckpoint(ctx, client, os.Args[2:])
	case "watch":
		return watchChats(ctx, client, os.Args[2:])
	default:
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}

type parseOptions struct {
	chatsPath       string
	checkpointsPath string
	matchesPath     string
	keywords        string
	limit           int
}

func listChats(ctx context.Context, client *tgclient.Client) error {
	dialogs, err := client.Dialogs(ctx, 100)
	if err != nil {
		return err
	}

	fmt.Printf("%-16s %-12s %-22s %-10s %-24s %s\n", "ID", "TYPE", "ACCESS_HASH", "UNREAD", "USERNAME", "TITLE")
	for _, dialog := range dialogs {
		fmt.Printf(
			"%-16d %-12s %-22d %-10d %-24s %s\n",
			dialog.ID,
			dialog.Type,
			dialog.AccessHash,
			dialog.Unread,
			dialog.Username,
			dialog.Title,
		)
	}

	return nil
}

func syncChats(ctx context.Context, client *tgclient.Client, args []string) error {
	fs := flag.NewFlagSet("sync-chats", flag.ContinueOnError)
	chatsPath := fs.String("chats", envOrDefault("CHATS_PATH", "data/chats.json"), "chats config path")
	limit := fs.Int("limit", envInt("DIALOGS_LIMIT", 100), "dialogs limit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dialogs, err := client.Dialogs(ctx, *limit)
	if err != nil {
		return err
	}

	store := filestorage.NewFileStore(*chatsPath, "", "")
	existing, _ := store.LoadChats(ctx)
	enabled := map[string]bool{}
	for _, chat := range existing {
		enabled[filestorage.CheckpointKey(chat)] = chat.Enabled
	}

	chats := make([]filestorage.Chat, 0, len(dialogs))
	for _, dialog := range dialogs {
		chat := filestorage.Chat{
			ID:         dialog.ID,
			Type:       dialog.Type,
			AccessHash: dialog.AccessHash,
			Title:      dialog.Title,
			Username:   dialog.Username,
			Enabled:    false,
		}
		if value, ok := enabled[filestorage.CheckpointKey(chat)]; ok {
			chat.Enabled = value
		}
		chats = append(chats, chat)
	}

	if err := store.SaveChats(ctx, chats); err != nil {
		return err
	}

	fmt.Printf("saved %d chats to %s\n", len(chats), *chatsPath)
	return nil
}

func showHistory(ctx context.Context, client *tgclient.Client, args []string) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	peerType := fs.String("type", "", "peer type: user, group, channel, supergroup")
	id := fs.Int64("id", 0, "peer id from list-chats")
	accessHash := fs.Int64("access-hash", 0, "peer access hash from list-chats")
	limit := fs.Int("limit", 20, "messages limit")
	keywords := fs.String("keywords", os.Getenv("KEYWORDS"), "comma-separated keywords")
	if err := fs.Parse(args); err != nil {
		return err
	}

	normalizedType := strings.ToLower(strings.TrimSpace(*peerType))
	if normalizedType == "" {
		return fmt.Errorf("--type is required")
	}
	if *id == 0 {
		return fmt.Errorf("--id is required")
	}

	messages, err := client.History(ctx, tgclient.PeerRef{
		ID:         *id,
		Type:       normalizedType,
		AccessHash: *accessHash,
	}, *limit)
	if err != nil {
		return err
	}

	matcher := msgparser.NewMatcher(splitKeywords(*keywords))
	if matcher.Empty() {
		printMessages(messages)
		return nil
	}

	printMatchedMessages(messages, matcher)

	return nil
}

func reparseChats(ctx context.Context, client *tgclient.Client, args []string) error {
	fs := flag.NewFlagSet("reparse", flag.ContinueOnError)
	peerType := fs.String("type", "", "peer type")
	id := fs.Int64("id", 0, "peer id")
	limit := fs.Int("limit", 100, "messages limit")
	chatsPath := fs.String("chats", envOrDefault("CHATS_PATH", "data/chats.json"), "chats config path")
	checkpointsPath := fs.String("checkpoints", envOrDefault("CHECKPOINTS_PATH", "data/checkpoints.json"), "checkpoints path")
	matchesPath := fs.String("matches", envOrDefault("MATCHES_PATH", "data/matches.jsonl"), "matches jsonl path")
	keywordsPath := fs.String("keywords-path", envOrDefault("KEYWORDS_PATH", "data/keywords.json"), "keywords path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	service := app.NewService(client, app.Config{
		ChatsPath:       *chatsPath,
		CheckpointsPath: *checkpointsPath,
		MatchesPath:     *matchesPath,
		KeywordsPath:    *keywordsPath,
	})

	if *peerType == "" || *id == 0 {
		summary, err := service.ReparseEnabled(ctx, app.ParseOptions{Limit: *limit}, nil)
		if err != nil {
			return err
		}
		fmt.Printf("reparse summary enabled=%d failed=%d matches=%d\n", summary.Enabled, summary.Failed, summary.Matches)
		return nil
	}

	summary, err := service.ReparseChat(ctx, strings.ToLower(*peerType), *id, app.ParseOptions{Limit: *limit}, nil)
	if err != nil {
		return err
	}
	fmt.Printf("reparse summary enabled=%d failed=%d matches=%d\n", summary.Enabled, summary.Failed, summary.Matches)
	return nil
}

func resetCheckpoint(ctx context.Context, client *tgclient.Client, args []string) error {
	fs := flag.NewFlagSet("reset-checkpoint", flag.ContinueOnError)
	peerType := fs.String("type", "", "peer type")
	id := fs.Int64("id", 0, "peer id")
	chatsPath := fs.String("chats", envOrDefault("CHATS_PATH", "data/chats.json"), "chats config path")
	checkpointsPath := fs.String("checkpoints", envOrDefault("CHECKPOINTS_PATH", "data/checkpoints.json"), "checkpoints path")
	matchesPath := fs.String("matches", envOrDefault("MATCHES_PATH", "data/matches.jsonl"), "matches jsonl path")
	keywordsPath := fs.String("keywords-path", envOrDefault("KEYWORDS_PATH", "data/keywords.json"), "keywords path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *peerType == "" || *id == 0 {
		return fmt.Errorf("--type and --id are required")
	}

	service := app.NewService(client, app.Config{
		ChatsPath:       *chatsPath,
		CheckpointsPath: *checkpointsPath,
		MatchesPath:     *matchesPath,
		KeywordsPath:    *keywordsPath,
	})

	chat, err := service.ResetCheckpoint(ctx, strings.ToLower(*peerType), *id)
	if err != nil {
		return err
	}
	fmt.Printf("checkpoint reset %s:%d %s\n", chat.Type, chat.ID, chat.Title)
	return nil
}

func parseChats(ctx context.Context, client *tgclient.Client, args []string) error {
	options, err := parseParseOptions("parse", args)
	if err != nil {
		return err
	}

	return runParse(ctx, client, options)
}

func watchChats(ctx context.Context, client *tgclient.Client, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	intervalValue := fs.String("interval", envOrDefault("WATCH_INTERVAL", "30s"), "parse interval")
	chatsPath := fs.String("chats", envOrDefault("CHATS_PATH", "data/chats.json"), "chats config path")
	checkpointsPath := fs.String("checkpoints", envOrDefault("CHECKPOINTS_PATH", "data/checkpoints.json"), "checkpoints path")
	matchesPath := fs.String("matches", envOrDefault("MATCHES_PATH", "data/matches.jsonl"), "matches jsonl path")
	keywords := fs.String("keywords", os.Getenv("KEYWORDS"), "comma-separated keywords")
	limit := fs.Int("limit", envInt("PARSE_LIMIT", 100), "messages limit per chat")
	if err := fs.Parse(args); err != nil {
		return err
	}

	interval, err := time.ParseDuration(*intervalValue)
	if err != nil {
		return fmt.Errorf("invalid watch interval: %w", err)
	}
	if interval <= 0 {
		return fmt.Errorf("watch interval must be positive")
	}

	options := parseOptions{
		chatsPath:       *chatsPath,
		checkpointsPath: *checkpointsPath,
		matchesPath:     *matchesPath,
		keywords:        *keywords,
		limit:           *limit,
	}

	for {
		if err := runParse(ctx, client, options); err != nil {
			log.Printf("parse cycle failed: %v", err)
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Printf("watch stopped: %v", ctx.Err())
			return nil
		case <-timer.C:
		}
	}
}

func parseParseOptions(name string, args []string) (parseOptions, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	chatsPath := fs.String("chats", envOrDefault("CHATS_PATH", "data/chats.json"), "chats config path")
	checkpointsPath := fs.String("checkpoints", envOrDefault("CHECKPOINTS_PATH", "data/checkpoints.json"), "checkpoints path")
	matchesPath := fs.String("matches", envOrDefault("MATCHES_PATH", "data/matches.jsonl"), "matches jsonl path")
	keywords := fs.String("keywords", os.Getenv("KEYWORDS"), "comma-separated keywords")
	limit := fs.Int("limit", envInt("PARSE_LIMIT", 100), "messages limit per chat")
	if err := fs.Parse(args); err != nil {
		return parseOptions{}, err
	}

	return parseOptions{
		chatsPath:       *chatsPath,
		checkpointsPath: *checkpointsPath,
		matchesPath:     *matchesPath,
		keywords:        *keywords,
		limit:           *limit,
	}, nil
}

func runParse(ctx context.Context, client *tgclient.Client, options parseOptions) error {
	matcher := msgparser.NewMatcher(splitKeywords(options.keywords))
	if matcher.Empty() {
		return fmt.Errorf("KEYWORDS or --keywords is required")
	}

	store := filestorage.NewFileStore(options.chatsPath, options.checkpointsPath, options.matchesPath)
	chats, err := store.LoadChats(ctx)
	if err != nil {
		return fmt.Errorf("load chats %s: %w", options.chatsPath, err)
	}
	checkpoints, err := store.LoadCheckpoints(ctx)
	if err != nil {
		return fmt.Errorf("load checkpoints %s: %w", options.checkpointsPath, err)
	}

	var enabledCount int
	var matchedCount int
	var failedCount int
	for _, chat := range chats {
		if !chat.Enabled {
			continue
		}
		enabledCount++

		if err := filestorage.ValidateChat(chat); err != nil {
			failedCount++
			log.Printf("chat config invalid %s err=%v", filestorage.ChatLogValue(chat), err)
			continue
		}

		matched, err := parseChat(ctx, client, store, checkpoints, chat, matcher, options.limit)
		if err != nil {
			failedCount++
			log.Printf("chat parse failed %s checkpoint=%d err=%v", filestorage.ChatLogValue(chat), checkpoints[filestorage.CheckpointKey(chat)], err)
			continue
		}
		matchedCount += matched

		if err := store.SaveCheckpoints(ctx, checkpoints); err != nil {
			return fmt.Errorf("save checkpoints %s after %s: %w", options.checkpointsPath, filestorage.ChatLogValue(chat), err)
		}
	}

	fmt.Printf("parse summary enabled=%d failed=%d matches=%d checkpoints=%s matches_file=%s\n", enabledCount, failedCount, matchedCount, options.checkpointsPath, options.matchesPath)
	return nil
}

func parseChat(
	ctx context.Context,
	client *tgclient.Client,
	store filestorage.FileStore,
	checkpoints map[string]int,
	chat filestorage.Chat,
	matcher msgparser.Matcher,
	limit int,
) (int, error) {
	key := filestorage.CheckpointKey(chat)
	checkpoint := checkpoints[key]

	messages, err := client.HistoryAfter(ctx, tgclient.PeerRef{
		ID:         chat.ID,
		Type:       chat.Type,
		AccessHash: chat.AccessHash,
	}, checkpoint, limit)
	if err != nil {
		return 0, fmt.Errorf("get history: %w", err)
	}

	sort.Slice(messages, func(i int, j int) bool {
		return messages[i].ID < messages[j].ID
	})

	log.Printf("chat history loaded %s checkpoint=%d messages=%d limit=%d", filestorage.ChatLogValue(chat), checkpoint, len(messages), limit)

	var matched int
	maxID := checkpoint
	for _, message := range messages {
		if message.ID <= checkpoint {
			continue
		}
		if message.ID > maxID {
			maxID = message.ID
		}

		text := oneLine(message.Text)
		if text == "" {
			continue
		}

		match, ok := matcher.Match(text)
		if !ok {
			continue
		}

		if err := store.AppendMatch(ctx, filestorage.Match{
			ChatID:    chat.ID,
			ChatType:  chat.Type,
			ChatTitle: chat.Title,
			MessageID: message.ID,
			Keyword:   match.Keyword,
			Text:      text,
			Date:      message.Date,
			ParsedAt:  time.Now(),
			Views:     message.Views,
		}); err != nil {
			return matched, fmt.Errorf("append match message_id=%d keyword=%q: %w", message.ID, match.Keyword, err)
		}

		matched++
		log.Printf("match found %s message_id=%d keyword=%q", filestorage.ChatLogValue(chat), message.ID, match.Keyword)
	}

	if maxID > checkpoint {
		checkpoints[key] = maxID
		log.Printf("checkpoint updated key=%s old=%d new=%d", key, checkpoint, maxID)
	}

	return matched, nil
}

func printMessages(messages []tgclient.Message) {
	fmt.Printf("%-8s %-20s %-6s %s\n", "ID", "DATE", "VIEWS", "TEXT")
	for _, message := range messages {
		fmt.Printf(
			"%-8d %-20s %-6d %s\n",
			message.ID,
			message.Date.Format("2006-01-02 15:04:05"),
			message.Views,
			oneLine(message.Text),
		)
	}
}

func printMatchedMessages(messages []tgclient.Message, matcher msgparser.Matcher) {
	fmt.Printf("%-8s %-20s %-16s %-6s %s\n", "ID", "DATE", "KEYWORD", "VIEWS", "TEXT")
	for _, message := range messages {
		match, ok := matcher.Match(message.Text)
		if !ok {
			continue
		}

		fmt.Printf(
			"%-8d %-20s %-16s %-6d %s\n",
			message.ID,
			message.Date.Format("2006-01-02 15:04:05"),
			match.Keyword,
			message.Views,
			oneLine(message.Text),
		)
	}
}

func splitKeywords(value string) []string {
	parts := strings.Split(value, ",")
	keywords := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		keywords = append(keywords, part)
	}

	return keywords
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}

	return parsed
}

func oneLine(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	return strings.TrimSpace(value)
}

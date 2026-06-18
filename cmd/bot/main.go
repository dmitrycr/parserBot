package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"parserTgChat/internal/app"
	tgbot "parserTgChat/internal/bot"
	tgclient "parserTgChat/internal/telegram"
)

func main() {
	if err := run(); err != nil {
		log.Printf("bot error: %v", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	if err := godotenv.Load(); err != nil {
		log.Printf("load .env: %v", err)
	}

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

	botToken, err := requiredEnv("TGBOT_TOKEN")
	if err != nil {
		return err
	}
	adminIDsValue, err := requiredEnv("BOT_ADMIN_IDS")
	if err != nil {
		return err
	}
	adminIDs, err := parseIDs(adminIDsValue)
	if err != nil {
		return fmt.Errorf("invalid BOT_ADMIN_IDS: %w", err)
	}

	client := tgclient.NewClient(apiID, apiHash, sessionPath)
	runBot := func(ctx context.Context, client *tgclient.Client) error {
		service := app.NewService(client, app.Config{
			ChatsPath:       envOrDefault("CHATS_PATH", "data/chats.json"),
			CheckpointsPath: envOrDefault("CHECKPOINTS_PATH", "data/checkpoints.json"),
			MatchesPath:     envOrDefault("MATCHES_PATH", "data/matches.jsonl"),
			KeywordsPath:    envOrDefault("KEYWORDS_PATH", "data/keywords.json"),
		})

		bot := tgbot.New(tgbot.Config{
			Token:             botToken,
			AdminIDs:          adminIDs,
			UsersPath:         envOrDefault("USERS_PATH", "data/users.json"),
			ParseLimit:        envInt("PARSE_LIMIT", 100),
			AutoParseEnabled:  envBool("AUTO_PARSE_ENABLED", true),
			AutoParseInterval: envDuration("AUTO_PARSE_INTERVAL", time.Hour),
		}, service)

		return bot.Run(ctx)
	}

	switch authMethod {
	case "qr":
		return client.RunQR(ctx, runBot)
	case "code":
		return client.Run(ctx, phone, runBot)
	default:
		return fmt.Errorf("unsupported TELEGRAM_AUTH_METHOD %q, use code or qr", authMethod)
	}
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

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}

	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}

	return parsed
}

func parseIDs(value string) ([]int64, error) {
	parts := strings.Split(value, ",")
	ids := make([]int64, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("empty ids list")
	}

	return ids, nil
}

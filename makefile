APP_NAME=parser
CMD=./cmd/parser
BOT_CMD=./cmd/bot
BIN=./bin/$(APP_NAME)
BOT_BIN=./bin/bot
GOCACHE?=/private/tmp/go-cache
LIMIT?=20
TYPE?=
ID?=
ACCESS_HASH?=0
KEYWORDS?=
CHATS?=data/chats.json
CHECKPOINTS?=data/checkpoints.json
MATCHES?=data/matches.jsonl
INTERVAL?=30s

.PHONY: run bot sync-chats list-chats history search parse reparse reset-checkpoint watch build build-bot test clean

run:
	GOCACHE=$(GOCACHE) go run $(CMD)

bot:
	GOCACHE=$(GOCACHE) go run $(BOT_CMD)

list-chats:
	GOCACHE=$(GOCACHE) go run $(CMD) list-chats

sync-chats:
	GOCACHE=$(GOCACHE) go run $(CMD) sync-chats --chats $(CHATS)

history:
	GOCACHE=$(GOCACHE) go run $(CMD) history --type $(TYPE) --id $(ID) --access-hash $(ACCESS_HASH) --limit $(LIMIT)

search:
	GOCACHE=$(GOCACHE) go run $(CMD) history --type $(TYPE) --id $(ID) --access-hash $(ACCESS_HASH) --limit $(LIMIT) --keywords "$(KEYWORDS)"

parse:
	GOCACHE=$(GOCACHE) go run $(CMD) parse --chats $(CHATS) --checkpoints $(CHECKPOINTS) --matches $(MATCHES) --limit $(LIMIT) --keywords "$(KEYWORDS)"

reparse:
	GOCACHE=$(GOCACHE) go run $(CMD) reparse --chats $(CHATS) --checkpoints $(CHECKPOINTS) --matches $(MATCHES) --limit $(LIMIT) --type $(TYPE) --id $(ID)

reset-checkpoint:
	GOCACHE=$(GOCACHE) go run $(CMD) reset-checkpoint --chats $(CHATS) --checkpoints $(CHECKPOINTS) --matches $(MATCHES) --type $(TYPE) --id $(ID)

watch:
	GOCACHE=$(GOCACHE) go run $(CMD) watch --chats $(CHATS) --checkpoints $(CHECKPOINTS) --matches $(MATCHES) --limit $(LIMIT) --keywords "$(KEYWORDS)" --interval $(INTERVAL)

build:
	GOCACHE=$(GOCACHE) go build -o $(BIN) $(CMD)

build-bot:
	GOCACHE=$(GOCACHE) go build -o $(BOT_BIN) $(BOT_CMD)

test:
	GOCACHE=$(GOCACHE) go test ./...

clean:
	rm -rf ./bin

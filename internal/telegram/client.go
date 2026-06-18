package telegram

import (
	"bufio"
	"context"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/session"
	tdtelegram "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"rsc.io/qr"
)

type Client struct {
	apiID       int
	apiHash     string
	sessionPath string

	tg *tdtelegram.Client
}

type Dialog struct {
	ID         int64
	Type       string
	AccessHash int64
	Title      string
	Username   string
	Unread     int
}

type PeerRef struct {
	ID         int64
	Type       string
	AccessHash int64
}

type Message struct {
	ID    int
	Date  time.Time
	Text  string
	Out   bool
	Views int
}

func NewClient(apiID int, apiHash string, sessionPath string) *Client {
	c := &Client{
		apiID:       apiID,
		apiHash:     apiHash,
		sessionPath: sessionPath,
	}
	c.tg = c.newTelegramClient()

	return c
}

func (c *Client) Connect(ctx context.Context, phone string) error {
	return c.Run(ctx, phone, func(ctx context.Context, client *Client) error {
		return nil
	})
}

func (c *Client) Run(ctx context.Context, phone string, fn func(ctx context.Context, client *Client) error) error {
	if err := c.ensureSessionDir(); err != nil {
		return err
	}

	c.tg = c.newTelegramClient()

	return c.tg.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(
			newConsoleAuth(phone),
			auth.SendCodeOptions{},
		)

		if err := c.tg.Auth().IfNecessary(ctx, flow); err != nil {
			return err
		}

		return fn(ctx, c)
	})
}

func (c *Client) ConnectQR(ctx context.Context) error {
	return c.RunQR(ctx, func(ctx context.Context, client *Client) error {
		return nil
	})
}

func (c *Client) RunQR(ctx context.Context, fn func(ctx context.Context, client *Client) error) error {
	if err := c.ensureSessionDir(); err != nil {
		return err
	}

	dispatcher := tg.NewUpdateDispatcher()
	loggedIn := qrlogin.OnLoginToken(dispatcher)

	c.tg = c.newTelegramClientWithOptions(tdtelegram.Options{
		SessionStorage: &session.FileStorage{
			Path: c.sessionPath,
		},
		UpdateHandler: dispatcher,
	})

	return c.tg.Run(ctx, func(ctx context.Context) error {
		status, err := c.tg.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if status.Authorized {
			return fn(ctx, c)
		}

		_, err = c.tg.QR().Auth(ctx, loggedIn, func(ctx context.Context, token qrlogin.Token) error {
			fmt.Printf("Open this URL in Telegram: %s\n", token.URL())

			qrPath, err := c.saveLoginQR(token)
			if err != nil {
				return err
			}
			fmt.Printf("QR saved to: %s\n", qrPath)

			return nil
		})

		if err != nil {
			return err
		}

		return fn(ctx, c)
	})
}

func (c *Client) API() *tg.Client {
	return c.tg.API()
}

func (c *Client) Dialogs(ctx context.Context, limit int) ([]Dialog, error) {
	if limit <= 0 {
		limit = 100
	}

	result, err := c.tg.API().MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      limit,
	})
	if err != nil {
		return nil, err
	}

	dialogs, err := parseDialogs(result)
	if err != nil {
		return nil, err
	}

	return dialogs, nil
}

func (c *Client) History(ctx context.Context, peer PeerRef, limit int) ([]Message, error) {
	return c.HistoryAfter(ctx, peer, 0, limit)
}

func (c *Client) HistoryAfter(ctx context.Context, peer PeerRef, minID int, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 20
	}

	inputPeer, err := peer.InputPeer()
	if err != nil {
		return nil, err
	}

	result, err := c.tg.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:  inputPeer,
		MinID: minID,
		Limit: limit,
	})
	if err != nil {
		return nil, err
	}

	return parseMessages(result)
}

func (p PeerRef) InputPeer() (tg.InputPeerClass, error) {
	if p.ID == 0 {
		return nil, fmt.Errorf("invalid peer: type=%s id=%d access_hash=%d: id is required", p.Type, p.ID, p.AccessHash)
	}

	switch p.Type {
	case "user":
		if p.AccessHash == 0 {
			return nil, fmt.Errorf("invalid peer: type=%s id=%d access_hash=%d: access_hash is required", p.Type, p.ID, p.AccessHash)
		}
		return &tg.InputPeerUser{
			UserID:     p.ID,
			AccessHash: p.AccessHash,
		}, nil
	case "group":
		return &tg.InputPeerChat{
			ChatID: p.ID,
		}, nil
	case "channel", "supergroup":
		if p.AccessHash == 0 {
			return nil, fmt.Errorf("invalid peer: type=%s id=%d access_hash=%d: access_hash is required", p.Type, p.ID, p.AccessHash)
		}
		return &tg.InputPeerChannel{
			ChannelID:  p.ID,
			AccessHash: p.AccessHash,
		}, nil
	default:
		return nil, fmt.Errorf("invalid peer: type=%s id=%d access_hash=%d: unsupported peer type", p.Type, p.ID, p.AccessHash)
	}
}

func (c *Client) newTelegramClient() *tdtelegram.Client {
	return c.newTelegramClientWithOptions(tdtelegram.Options{
		SessionStorage: &session.FileStorage{Path: c.sessionPath},
	})
}

func (c *Client) newTelegramClientWithOptions(options tdtelegram.Options) *tdtelegram.Client {
	return tdtelegram.NewClient(c.apiID, c.apiHash, options)
}

func (c *Client) ensureSessionDir() error {
	dir := filepath.Dir(c.sessionPath)
	if dir == "." || dir == "" {
		return nil
	}

	return os.MkdirAll(dir, 0700)
}

func (c *Client) saveLoginQR(token qrlogin.Token) (string, error) {
	image, err := token.Image(qr.M)
	if err != nil {
		return "", err
	}

	path := filepath.Join(filepath.Dir(c.sessionPath), "login_qr.png")
	if filepath.Dir(c.sessionPath) == "." {
		path = "login_qr.png"
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	defer file.Close()

	if err := png.Encode(file, image); err != nil {
		return "", err
	}

	return path, nil
}

type dialogsResponse interface {
	GetDialogs() []tg.DialogClass
	GetChats() []tg.ChatClass
	GetUsers() []tg.UserClass
}

type messagesResponse interface {
	GetMessages() []tg.MessageClass
}

func parseDialogs(result tg.MessagesDialogsClass) ([]Dialog, error) {
	response, ok := result.(dialogsResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected dialogs response type %T", result)
	}

	users := indexUsers(response.GetUsers())
	chats := indexChats(response.GetChats())

	dialogs := make([]Dialog, 0, len(response.GetDialogs()))
	for _, item := range response.GetDialogs() {
		dialog, ok := item.(*tg.Dialog)
		if !ok {
			continue
		}

		parsed := dialogFromPeer(dialog.Peer, users, chats)
		parsed.Unread = dialog.UnreadCount
		dialogs = append(dialogs, parsed)
	}

	return dialogs, nil
}

func indexUsers(users []tg.UserClass) map[int64]*tg.User {
	index := make(map[int64]*tg.User, len(users))
	for _, item := range users {
		user, ok := item.(*tg.User)
		if !ok {
			continue
		}
		index[user.ID] = user
	}

	return index
}

func indexChats(chats []tg.ChatClass) map[int64]tg.ChatClass {
	index := make(map[int64]tg.ChatClass, len(chats))
	for _, item := range chats {
		switch chat := item.(type) {
		case *tg.Chat:
			index[chat.ID] = chat
		case *tg.ChatForbidden:
			index[chat.ID] = chat
		case *tg.Channel:
			index[chat.ID] = chat
		case *tg.ChannelForbidden:
			index[chat.ID] = chat
		}
	}

	return index
}

func dialogFromPeer(peer tg.PeerClass, users map[int64]*tg.User, chats map[int64]tg.ChatClass) Dialog {
	switch p := peer.(type) {
	case *tg.PeerUser:
		user := users[p.UserID]
		return Dialog{
			ID:         p.UserID,
			Type:       "user",
			AccessHash: userAccessHash(user),
			Title:      userTitle(user),
			Username:   userUsername(user),
		}
	case *tg.PeerChat:
		return Dialog{
			ID:    p.ChatID,
			Type:  "group",
			Title: chatTitle(chats[p.ChatID]),
		}
	case *tg.PeerChannel:
		chat := chats[p.ChannelID]
		return Dialog{
			ID:         p.ChannelID,
			Type:       channelType(chat),
			AccessHash: channelAccessHash(chat),
			Title:      chatTitle(chat),
			Username:   channelUsername(chat),
		}
	default:
		return Dialog{Type: fmt.Sprintf("%T", peer)}
	}
}

func parseMessages(result tg.MessagesMessagesClass) ([]Message, error) {
	response, ok := result.(messagesResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected history response type %T", result)
	}

	messages := make([]Message, 0, len(response.GetMessages()))
	for _, item := range response.GetMessages() {
		message, ok := item.(*tg.Message)
		if !ok {
			continue
		}

		messages = append(messages, Message{
			ID:    message.ID,
			Date:  time.Unix(int64(message.Date), 0),
			Text:  message.Message,
			Out:   message.Out,
			Views: message.Views,
		})
	}

	return messages, nil
}

func userAccessHash(user *tg.User) int64 {
	if user == nil {
		return 0
	}

	return user.AccessHash
}

func userTitle(user *tg.User) string {
	if user == nil {
		return ""
	}

	parts := make([]string, 0, 2)
	if user.FirstName != "" {
		parts = append(parts, user.FirstName)
	}
	if user.LastName != "" {
		parts = append(parts, user.LastName)
	}
	if len(parts) == 0 {
		return user.Username
	}

	return strings.Join(parts, " ")
}

func userUsername(user *tg.User) string {
	if user == nil {
		return ""
	}

	return user.Username
}

func chatTitle(chat tg.ChatClass) string {
	switch c := chat.(type) {
	case *tg.Chat:
		return c.Title
	case *tg.ChatForbidden:
		return c.Title
	case *tg.Channel:
		return c.Title
	case *tg.ChannelForbidden:
		return c.Title
	default:
		return ""
	}
}

func channelType(chat tg.ChatClass) string {
	channel, ok := chat.(*tg.Channel)
	if !ok {
		return "channel"
	}
	if channel.Megagroup {
		return "supergroup"
	}
	if channel.Broadcast {
		return "channel"
	}

	return "channel"
}

func channelUsername(chat tg.ChatClass) string {
	channel, ok := chat.(*tg.Channel)
	if !ok {
		return ""
	}

	return channel.Username
}

func channelAccessHash(chat tg.ChatClass) int64 {
	switch c := chat.(type) {
	case *tg.Channel:
		return c.AccessHash
	case *tg.ChannelForbidden:
		return c.AccessHash
	default:
		return 0
	}
}

type consoleAuth struct {
	phone string
	input *bufio.Reader
}

func newConsoleAuth(phone string) consoleAuth {
	return consoleAuth{
		phone: phone,
		input: bufio.NewReader(os.Stdin),
	}
}

func (a consoleAuth) Phone(ctx context.Context) (string, error) {
	return a.phone, nil
}

func (a consoleAuth) Password(ctx context.Context) (string, error) {
	return a.readLine("Enter Telegram 2FA password: ")
}

func (a consoleAuth) Code(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
	fmt.Printf("Telegram code sent via: %T\n", sentCode.Type)

	return a.readLine("Enter Telegram code: ")
}

func (a consoleAuth) AcceptTermsOfService(ctx context.Context, tos tg.HelpTermsOfService) error {
	return fmt.Errorf("telegram sign up is required, but this client supports only existing accounts")
}

func (a consoleAuth) SignUp(ctx context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, fmt.Errorf("telegram sign up is not supported")
}

func (a consoleAuth) readLine(prompt string) (string, error) {
	fmt.Print(prompt)

	value, err := a.input.ReadString('\n')
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(value), nil
}

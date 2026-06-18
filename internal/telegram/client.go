package telegram

import (
	"bufio"
	"context"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gotd/td/session"
	tdtelegram "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
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
	FolderID   int
}

type DialogFolder struct {
	ID    int
	Title string
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
			if !tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
				return err
			}

			password, passwordErr := newConsoleAuth("").Password(ctx)
			if passwordErr != nil {
				return passwordErr
			}
			if _, passwordErr := c.tg.Auth().Password(ctx, password); passwordErr != nil {
				return passwordErr
			}
		}

		return fn(ctx, c)
	})
}

func (c *Client) API() *tg.Client {
	return c.tg.API()
}

func (c *Client) Dialogs(ctx context.Context, limit int) ([]Dialog, error) {
	return c.dialogs(ctx, limit)
}

func (c *Client) DialogsInFolder(ctx context.Context, folderID int, limit int) ([]Dialog, error) {
	if folderID <= 0 {
		return nil, fmt.Errorf("invalid folder id: %d", folderID)
	}

	filter, err := c.dialogFilter(ctx, folderID)
	if err != nil {
		return nil, err
	}

	dialogs, err := c.dialogs(ctx, limit)
	if err != nil {
		return nil, err
	}

	filtered := make([]Dialog, 0, len(dialogs))
	for _, dialog := range dialogs {
		if filter.Match(dialog) {
			filtered = append(filtered, dialog)
		}
	}

	return filtered, nil
}

func (c *Client) DialogFolders(ctx context.Context) ([]DialogFolder, error) {
	result, err := c.tg.API().MessagesGetDialogFilters(ctx)
	if err != nil {
		return nil, err
	}

	folders := make([]DialogFolder, 0, len(result.Filters))
	for _, item := range result.Filters {
		switch folder := item.(type) {
		case *tg.DialogFilter:
			folders = append(folders, dialogFolder(folder.ID, folder.Title.Text))
		case *tg.DialogFilterChatlist:
			folders = append(folders, dialogFolder(folder.ID, folder.Title.Text))
		}
	}
	sort.Slice(folders, func(i, j int) bool {
		return folders[i].ID < folders[j].ID
	})

	return folders, nil
}

func (c *Client) dialogFilter(ctx context.Context, folderID int) (dialogFilterMatcher, error) {
	result, err := c.tg.API().MessagesGetDialogFilters(ctx)
	if err != nil {
		return dialogFilterMatcher{}, err
	}

	for _, item := range result.Filters {
		switch folder := item.(type) {
		case *tg.DialogFilter:
			if folder.ID == folderID {
				return newDialogFilterMatcher(
					folderID,
					folder.Groups,
					folder.Broadcasts,
					folder.Bots,
					folder.PinnedPeers,
					folder.IncludePeers,
					folder.ExcludePeers,
				), nil
			}
		case *tg.DialogFilterChatlist:
			if folder.ID == folderID {
				return newDialogFilterMatcher(
					folderID,
					false,
					false,
					false,
					folder.PinnedPeers,
					folder.IncludePeers,
					nil,
				), nil
			}
		}
	}

	return dialogFilterMatcher{}, fmt.Errorf("folder not found: %d", folderID)
}

func (c *Client) dialogs(ctx context.Context, limit int) ([]Dialog, error) {
	if limit <= 0 {
		limit = 100
	}

	const batchSize = 100

	dialogs := make([]Dialog, 0, limit)
	offset := dialogOffset{peer: &tg.InputPeerEmpty{}}
	for len(dialogs) < limit {
		currentLimit := batchSize
		if remaining := limit - len(dialogs); remaining < currentLimit {
			currentLimit = remaining
		}

		result, err := c.tg.API().MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetPeer: offset.peer,
			OffsetID:   offset.id,
			OffsetDate: offset.date,
			Limit:      currentLimit,
		})
		if err != nil {
			return nil, err
		}

		page, err := parseDialogsPage(result)
		if err != nil {
			return nil, err
		}
		if len(page.dialogs) == 0 {
			break
		}

		dialogs = append(dialogs, page.dialogs...)
		if page.last || page.next.peer == nil || offset.same(page.next) {
			break
		}
		offset = page.next
	}

	return dialogs, nil
}

type dialogOffset struct {
	peer tg.InputPeerClass
	id   int
	date int
}

func (o dialogOffset) same(next dialogOffset) bool {
	if o.id != next.id || o.date != next.date {
		return false
	}
	return inputPeerKey(o.peer) == inputPeerKey(next.peer)
}

type dialogsPage struct {
	dialogs []Dialog
	next    dialogOffset
	last    bool
}

type dialogFilterMatcher struct {
	folderID   int
	groups     bool
	broadcasts bool
	bots       bool
	include    map[string]struct{}
	exclude    map[string]struct{}
}

func newDialogFilterMatcher(folderID int, groups bool, broadcasts bool, bots bool, pinned []tg.InputPeerClass, include []tg.InputPeerClass, exclude []tg.InputPeerClass) dialogFilterMatcher {
	matcher := dialogFilterMatcher{
		folderID:   folderID,
		groups:     groups,
		broadcasts: broadcasts,
		bots:       bots,
		include:    map[string]struct{}{},
		exclude:    map[string]struct{}{},
	}
	for _, peer := range pinned {
		if key := inputPeerKey(peer); key != "" {
			matcher.include[key] = struct{}{}
		}
	}
	for _, peer := range include {
		if key := inputPeerKey(peer); key != "" {
			matcher.include[key] = struct{}{}
		}
	}
	for _, peer := range exclude {
		if key := inputPeerKey(peer); key != "" {
			matcher.exclude[key] = struct{}{}
		}
	}

	return matcher
}

func (m dialogFilterMatcher) Match(dialog Dialog) bool {
	key := dialogKey(dialog)
	if _, ok := m.exclude[key]; ok {
		return false
	}
	if _, ok := m.include[key]; ok {
		return true
	}
	if dialog.FolderID == m.folderID {
		return true
	}
	if m.groups && (dialog.Type == "group" || dialog.Type == "supergroup") {
		return true
	}
	if m.broadcasts && dialog.Type == "channel" {
		return true
	}
	if m.bots && dialog.Type == "bot" {
		return true
	}

	return false
}

func inputPeerKey(peer tg.InputPeerClass) string {
	switch p := peer.(type) {
	case *tg.InputPeerUser:
		return fmt.Sprintf("user:%d", p.UserID)
	case *tg.InputPeerChat:
		return fmt.Sprintf("group:%d", p.ChatID)
	case *tg.InputPeerChannel:
		return fmt.Sprintf("channel:%d", p.ChannelID)
	default:
		return ""
	}
}

func dialogKey(dialog Dialog) string {
	switch dialog.Type {
	case "user", "bot":
		return fmt.Sprintf("user:%d", dialog.ID)
	case "group":
		return fmt.Sprintf("group:%d", dialog.ID)
	case "channel", "supergroup":
		return fmt.Sprintf("channel:%d", dialog.ID)
	default:
		return fmt.Sprintf("%s:%d", dialog.Type, dialog.ID)
	}
}

func peerKey(peer tg.PeerClass) string {
	switch p := peer.(type) {
	case *tg.PeerUser:
		return fmt.Sprintf("user:%d", p.UserID)
	case *tg.PeerChat:
		return fmt.Sprintf("group:%d", p.ChatID)
	case *tg.PeerChannel:
		return fmt.Sprintf("channel:%d", p.ChannelID)
	default:
		return ""
	}
}

func dialogFolder(id int, title string) DialogFolder {
	if title == "" {
		title = fmt.Sprintf("Папка %d", id)
	}

	return DialogFolder{
		ID:    id,
		Title: title,
	}
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
	GetMessages() []tg.MessageClass
}

type messagesResponse interface {
	GetMessages() []tg.MessageClass
}

func parseDialogs(result tg.MessagesDialogsClass) ([]Dialog, error) {
	page, err := parseDialogsPage(result)
	if err != nil {
		return nil, err
	}

	return page.dialogs, nil
}

func parseDialogsPage(result tg.MessagesDialogsClass) (dialogsPage, error) {
	response, ok := result.(dialogsResponse)
	if !ok {
		return dialogsPage{}, fmt.Errorf("unexpected dialogs response type %T", result)
	}

	users := indexUsers(response.GetUsers())
	chats := indexChats(response.GetChats())
	offsets := indexMessageOffsets(response.GetMessages())

	dialogs := make([]Dialog, 0, len(response.GetDialogs()))
	var next dialogOffset
	for _, item := range response.GetDialogs() {
		dialog, ok := item.(*tg.Dialog)
		if !ok {
			continue
		}

		parsed := dialogFromPeer(dialog.Peer, users, chats)
		parsed.Unread = dialog.UnreadCount
		if folderID, ok := dialog.GetFolderID(); ok {
			parsed.FolderID = folderID
		}
		dialogs = append(dialogs, parsed)

		if offset, ok := offsetForDialog(dialog, parsed, offsets); ok {
			next = offset
		}
	}

	return dialogsPage{
		dialogs: dialogs,
		next:    next,
		last:    isLastDialogsPage(result),
	}, nil
}

type messageOffset struct {
	id   int
	date int
}

func indexMessageOffsets(messages []tg.MessageClass) map[string]messageOffset {
	offsets := make(map[string]messageOffset, len(messages))
	for _, item := range messages {
		message, ok := item.AsNotEmpty()
		if !ok {
			continue
		}
		offsets[peerKey(message.GetPeerID())] = messageOffset{
			id:   message.GetID(),
			date: message.GetDate(),
		}
	}

	return offsets
}

func offsetForDialog(dialog *tg.Dialog, parsed Dialog, offsets map[string]messageOffset) (dialogOffset, bool) {
	inputPeer, err := (PeerRef{
		ID:         parsed.ID,
		Type:       parsed.Type,
		AccessHash: parsed.AccessHash,
	}).InputPeer()
	if err != nil {
		return dialogOffset{}, false
	}

	offset, ok := offsets[peerKey(dialog.Peer)]
	if !ok {
		offset = messageOffset{id: dialog.TopMessage}
	}

	return dialogOffset{
		peer: inputPeer,
		id:   offset.id,
		date: offset.date,
	}, true
}

func isLastDialogsPage(result tg.MessagesDialogsClass) bool {
	switch result.(type) {
	case *tg.MessagesDialogs:
		return true
	default:
		return false
	}
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

package textmsg

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	MaxContentBytes      = 64 * 1024 // 64 KiB
	MaxHistoryPerSession = 100
)

var (
	ErrEmptyContent    = errors.New("textmsg: content is empty")
	ErrContentTooLarge = errors.New("textmsg: content exceeds maximum allowed size")
	ErrInvalidUTF8     = errors.New("textmsg: content contains invalid UTF-8")
	ErrInvalidSender   = errors.New("textmsg: invalid sender; must be 'desktop' or 'phone'")
)

type Message struct {
	ID        string    `json:"id"`
	Sender    string    `json:"sender"` // "desktop" or "phone"
	Content   string    `json:"content"`
	IsLink    bool      `json:"isLink"`
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

func IsSafeURL(text string) (bool, string) {
	trimmed := strings.TrimSpace(text)
	if strings.ContainsAny(trimmed, "\r\n\t ") {
		return false, ""
	}
	u, err := url.ParseRequestURI(trimmed)
	if err != nil {
		return false, ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false, ""
	}
	if u.Host == "" {
		return false, ""
	}
	return true, trimmed
}

type Store struct {
	mu       sync.RWMutex
	messages map[string][]*Message // keyed by session key
}

func NewStore() *Store {
	return &Store{
		messages: make(map[string][]*Message),
	}
}

func (s *Store) Add(sessionKey, sender, content string) (*Message, error) {
	if sender != "desktop" && sender != "phone" {
		return nil, ErrInvalidSender
	}

	content = strings.TrimSpace(content)
	if content == "" {
		return nil, ErrEmptyContent
	}
	if len(content) > MaxContentBytes {
		return nil, ErrContentTooLarge
	}
	if !utf8.ValidString(content) {
		return nil, ErrInvalidUTF8
	}

	buf := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(buf)

	isLink, targetURL := IsSafeURL(content)

	msg := &Message{
		ID:        id,
		Sender:    sender,
		Content:   content,
		IsLink:    isLink,
		URL:       targetURL,
		CreatedAt: time.Now(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	list := s.messages[sessionKey]
	list = append(list, msg)
	if len(list) > MaxHistoryPerSession {
		list = list[len(list)-MaxHistoryPerSession:]
	}
	s.messages[sessionKey] = list
	return msg, nil
}

func (s *Store) List(sessionKey string) []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()

	msgs := s.messages[sessionKey]
	res := make([]Message, len(msgs))
	for i, m := range msgs {
		res[i] = *m
	}
	return res
}

func (s *Store) Clear(sessionKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.messages, sessionKey)
}

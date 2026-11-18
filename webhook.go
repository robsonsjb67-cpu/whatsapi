package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

// WebhookSender envia eventos em background, com algumas tentativas.
type WebhookSender struct {
	secret string
	client *http.Client
	queue  chan webhookJob
}

type webhookJob struct {
	url  string
	body []byte
}

func NewWebhookSender(secret string) *WebhookSender {
	w := &WebhookSender{
		secret: secret,
		client: &http.Client{Timeout: 15 * time.Second},
		queue:  make(chan webhookJob, 1000),
	}
	for range 4 {
		go w.worker()
	}
	return w
}

func (w *WebhookSender) Send(url string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("webhook: erro ao serializar: %v", err)
		return
	}
	select {
	case w.queue <- webhookJob{url, body}:
	default:
		log.Printf("webhook: fila cheia, evento descartado")
	}
}

func (w *WebhookSender) worker() {
	for job := range w.queue {
		backoff := time.Second
		for attempt := 1; attempt <= 3; attempt++ {
			if w.post(job) {
				break
			}
			if attempt < 3 {
				time.Sleep(backoff)
				backoff *= 3
			} else {
				log.Printf("webhook: desistindo após 3 tentativas para %s", job.url)
			}
		}
	}
}

func (w *WebhookSender) post(job webhookJob) bool {
	req, err := http.NewRequest(http.MethodPost, job.url, bytes.NewReader(job.body))
	if err != nil {
		log.Printf("webhook: URL inválida %q: %v", job.url, err)
		return true // não adianta tentar de novo
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "WhatsAPI-Webhook/1.0")
	if w.secret != "" {
		mac := hmac.New(sha256.New, []byte(w.secret))
		mac.Write(job.body)
		req.Header.Set("X-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := w.client.Do(req)
	if err != nil {
		log.Printf("webhook: %v", err)
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// messagePayload converte uma mensagem recebida num JSON simples.
func (i *Instance) messagePayload(evt *events.Message) map[string]any {
	info := evt.Info
	data := map[string]any{
		"id":        info.ID,
		"chat":      info.Chat.String(),
		"sender":    info.Sender.ToNonAD().String(),
		"from":      info.Sender.User,
		"pushName":  info.PushName,
		"fromMe":    info.IsFromMe,
		"isGroup":   info.IsGroup,
		"timestamp": info.Timestamp.Unix(),
	}
	if !info.SenderAlt.IsEmpty() {
		data["senderAlt"] = info.SenderAlt.ToNonAD().String()
	}
	msg := evt.Message
	var media whatsmeow.DownloadableMessage
	var mime string

	switch {
	case msg.GetConversation() != "":
		data["type"] = "text"
		data["text"] = msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		data["type"] = "text"
		data["text"] = msg.GetExtendedTextMessage().GetText()
		if ctx := msg.GetExtendedTextMessage().GetContextInfo(); ctx != nil && ctx.GetStanzaID() != "" {
			data["quotedId"] = ctx.GetStanzaID()
		}
	case msg.GetImageMessage() != nil:
		m := msg.GetImageMessage()
		data["type"], data["caption"] = "image", m.GetCaption()
		media, mime = m, m.GetMimetype()
	case msg.GetVideoMessage() != nil:
		m := msg.GetVideoMessage()
		data["type"], data["caption"] = "video", m.GetCaption()
		media, mime = m, m.GetMimetype()
	case msg.GetAudioMessage() != nil:
		m := msg.GetAudioMessage()
		data["type"], data["ptt"], data["seconds"] = "audio", m.GetPTT(), m.GetSeconds()
		media, mime = m, m.GetMimetype()
	case msg.GetDocumentMessage() != nil:
		m := msg.GetDocumentMessage()
		data["type"], data["fileName"], data["caption"] = "document", m.GetFileName(), m.GetCaption()
		media, mime = m, m.GetMimetype()
	case msg.GetStickerMessage() != nil:
		m := msg.GetStickerMessage()
		data["type"] = "sticker"
		media, mime = m, m.GetMimetype()
	case msg.GetLocationMessage() != nil:
		m := msg.GetLocationMessage()
		data["type"] = "location"
		data["latitude"], data["longitude"] = m.GetDegreesLatitude(), m.GetDegreesLongitude()
		data["name"], data["address"] = m.GetName(), m.GetAddress()
	case msg.GetContactMessage() != nil:
		data["type"] = "contact"
		data["displayName"] = msg.GetContactMessage().GetDisplayName()
		data["vcard"] = msg.GetContactMessage().GetVcard()
	case msg.GetReactionMessage() != nil:
		data["type"] = "reaction"
		data["emoji"] = msg.GetReactionMessage().GetText()
		data["reactedId"] = msg.GetReactionMessage().GetKey().GetID()
	case msg.GetButtonsResponseMessage() != nil:
		data["type"] = "button_reply"
		data["buttonId"] = msg.GetButtonsResponseMessage().GetSelectedButtonID()
		data["text"] = msg.GetButtonsResponseMessage().GetSelectedDisplayText()
	case msg.GetListResponseMessage() != nil:
		data["type"] = "list_reply"
		data["rowId"] = msg.GetListResponseMessage().GetSingleSelectReply().GetSelectedRowID()
		data["text"] = msg.GetListResponseMessage().GetTitle()
	default:
		data["type"] = "other"
	}

	if media != nil {
		data["mimetype"] = mime
		if i.mgr.cfg.MediaBase64 {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if b, err := i.Client().Download(ctx, media); err == nil {
				data["base64"] = base64.StdEncoding.EncodeToString(b)
			} else {
				data["mediaError"] = err.Error()
			}
		}
	}
	return data
}

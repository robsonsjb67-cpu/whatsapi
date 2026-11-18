package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

const maxMediaBytes = 64 << 20 // 64 MB

type API struct {
	mgr *Manager
	cfg Config
}

//go:embed panel.html
var panelHTML []byte

func NewRouter(mgr *Manager, cfg Config) http.Handler {
	a := &API{mgr: mgr, cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(panelHTML)
	})
	// Entrada automática no painel quando aberto no próprio computador.
	mux.HandleFunc("GET /panel/token", func(w http.ResponseWriter, r *http.Request) {
		if !isLocalRequest(r) {
			writeErr(w, http.StatusForbidden, errors.New("só funciona abrindo o painel no próprio computador"))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]any{"token": a.cfg.APIToken})
	})
	a.registerEvolution(mux)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /instances", a.auth(a.listInstances))
	mux.HandleFunc("POST /instances", a.auth(a.createInstance))
	mux.HandleFunc("GET /instances/{name}", a.auth(a.withInstance(a.getStatus)))
	mux.HandleFunc("DELETE /instances/{name}", a.auth(a.deleteInstance))
	mux.HandleFunc("POST /instances/{name}/connect", a.auth(a.withInstance(a.connect)))
	mux.HandleFunc("GET /instances/{name}/qr", a.auth(a.withInstance(a.getQR)))
	mux.HandleFunc("POST /instances/{name}/paircode", a.auth(a.withInstance(a.pairCode)))
	mux.HandleFunc("POST /instances/{name}/logout", a.auth(a.withInstance(a.logout)))
	mux.HandleFunc("PUT /instances/{name}/webhook", a.auth(a.withInstance(a.setWebhook)))

	mux.HandleFunc("POST /instances/{name}/send/text", a.auth(a.withInstance(a.sendText)))
	mux.HandleFunc("POST /instances/{name}/send/media", a.auth(a.withInstance(a.sendMedia)))
	mux.HandleFunc("POST /instances/{name}/send/location", a.auth(a.withInstance(a.sendLocation)))
	mux.HandleFunc("POST /instances/{name}/send/reaction", a.auth(a.withInstance(a.sendReaction)))
	mux.HandleFunc("POST /instances/{name}/campaign", a.auth(a.withInstance(a.startCampaign)))
	mux.HandleFunc("GET /instances/{name}/campaign", a.auth(a.withInstance(a.getCampaign)))
	mux.HandleFunc("DELETE /instances/{name}/campaign", a.auth(a.withInstance(a.stopCampaign)))
	mux.HandleFunc("POST /instances/{name}/check", a.auth(a.withInstance(a.checkNumbers)))
	mux.HandleFunc("GET /instances/{name}/groups", a.auth(a.withInstance(a.listGroups)))
	return mux
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxMediaBytes*2)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("JSON inválido: %w", err)
	}
	return nil
}

func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimSpace(r.Header.Get("token"))
		if tok == "" {
			tok = strings.TrimSpace(r.Header.Get("apikey")) // compatível com Evolution/WaDuck
		}
		if tok == "" {
			tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if tok == "" { // permite abrir o QR no navegador: /instances/x/qr?format=png&token=...
			tok = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(a.cfg.APIToken)) != 1 {
			writeErr(w, http.StatusUnauthorized, errors.New("token inválido"))
			return
		}
		next(w, r)
	}
}

// isLocalRequest: conexão vindo do próprio PC, com Host localhost/127.0.0.1
// (o Host impede que um site externo use DNS rebinding) e sem proxy na frente.
func isLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" || r.Header.Get("Cf-Connecting-Ip") != "" {
		return false
	}
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if h != "localhost" && h != "127.0.0.1" && h != "::1" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" && !strings.Contains(o, "://localhost") && !strings.Contains(o, "://127.0.0.1") && !strings.Contains(o, "://[::1]") {
		return false
	}
	return true
}

type instHandler func(w http.ResponseWriter, r *http.Request, inst *Instance)

func (a *API) withInstance(next instHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, err := a.mgr.Get(r.PathValue("name"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		next(w, r, inst)
	}
}

func requireLogin(w http.ResponseWriter, inst *Instance) (*whatsmeow.Client, bool) {
	cli := inst.Client()
	if !cli.IsLoggedIn() {
		writeErr(w, http.StatusConflict, ErrNotLoggedIn)
		return nil, false
	}
	return cli, true
}

// resolveJID aceita "5511999999999", "+55 (11) 99999-9999", "xxx@s.whatsapp.net" ou "xxx@g.us".
// Para números, consulta o WhatsApp para obter o JID real (resolve o problema do 9º dígito no Brasil).
func resolveJID(ctx context.Context, cli *whatsmeow.Client, to string) (types.JID, error) {
	to = strings.TrimSpace(to)
	if strings.Contains(to, "@") {
		return types.ParseJID(to)
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, to)
	if len(digits) < 8 {
		return types.JID{}, fmt.Errorf("número inválido: %q", to)
	}
	res, err := cli.IsOnWhatsApp(ctx, []string{"+" + digits})
	if err != nil {
		return types.JID{}, fmt.Errorf("erro ao verificar número: %w", err)
	}
	for _, r := range res {
		if r.IsIn {
			return r.JID, nil
		}
	}
	return types.JID{}, fmt.Errorf("o número %s não está no WhatsApp", digits)
}

func sendResult(w http.ResponseWriter, resp whatsmeow.SendResponse, to types.JID, err error) {
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": resp.ID, "to": to.String(), "timestamp": resp.Timestamp.Unix()})
}

func contextInfo(quotedID string, mentions []string) *waE2E.ContextInfo {
	if quotedID == "" && len(mentions) == 0 {
		return nil
	}
	ci := &waE2E.ContextInfo{}
	if quotedID != "" {
		ci.StanzaID = proto.String(quotedID)
		ci.QuotedMessage = &waE2E.Message{Conversation: proto.String("")}
	}
	for _, m := range mentions {
		if !strings.Contains(m, "@") {
			m += "@" + types.DefaultUserServer
		}
		ci.MentionedJID = append(ci.MentionedJID, m)
	}
	return ci
}

// ---------- instâncias ----------

func (a *API) listInstances(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.mgr.List())
}

func (a *API) createInstance(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Webhook string `json:"webhook"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	inst, err := a.mgr.Create(r.Context(), body.Name, body.Webhook)
	switch {
	case errors.Is(err, ErrExists):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, ErrInvalidName):
		writeErr(w, 400, err)
	case err != nil:
		writeErr(w, 500, err)
	default:
		writeJSON(w, 201, inst.Status())
	}
}

func (a *API) getStatus(w http.ResponseWriter, r *http.Request, inst *Instance) {
	writeJSON(w, 200, inst.Status())
}

func (a *API) deleteInstance(w http.ResponseWriter, r *http.Request) {
	if err := a.mgr.Delete(r.Context(), r.PathValue("name")); err != nil {
		code := 500
		if errors.Is(err, ErrNotFound) {
			code = 404
		}
		writeErr(w, code, err)
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": true})
}

func (a *API) connect(w http.ResponseWriter, r *http.Request, inst *Instance) {
	if err := inst.Connect(); err != nil {
		writeErr(w, 500, err)
		return
	}
	inst.QR(5 * time.Second)
	a.getQR(w, r, inst)
}

// getQR devolve o QR atual em texto e como PNG base64.
// Use ?format=png para receber a imagem direto (abra no navegador).
func (a *API) getQR(w http.ResponseWriter, r *http.Request, inst *Instance) {
	st := inst.Status()
	if st.LoggedIn {
		writeJSON(w, 200, map[string]any{"status": st.Status, "message": "já conectado", "instance": st})
		return
	}
	qr := inst.QR(5 * time.Second)
	if qr == "" {
		writeJSON(w, 200, map[string]any{"status": st.Status, "message": "sem QR ativo; chame POST /instances/{name}/connect", "instance": st})
		return
	}
	png, err := qrcode.Encode(qr, qrcode.Medium, 512)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if r.URL.Query().Get("format") == "png" {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(png)
		return
	}
	writeJSON(w, 200, map[string]any{
		"status": "qr",
		"qr":     qr,
		"base64": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
	})
}

func (a *API) pairCode(w http.ResponseWriter, r *http.Request, inst *Instance) {
	var body struct {
		Phone string `json:"phone"`
	}
	if err := decode(r, &body); err != nil || body.Phone == "" {
		writeErr(w, 400, errors.New(`informe {"phone": "5511999999999"}`))
		return
	}
	code, err := inst.PairCode(r.Context(), body.Phone)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": code, "message": "No celular: Aparelhos conectados > Conectar com número de telefone"})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request, inst *Instance) {
	if err := inst.Logout(r.Context()); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, inst.Status())
}

func (a *API) setWebhook(w http.ResponseWriter, r *http.Request, inst *Instance) {
	var body struct {
		URL string `json:"url"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := inst.SetWebhook(r.Context(), body.URL); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, inst.Status())
}

// ---------- envio ----------

func (a *API) sendText(w http.ResponseWriter, r *http.Request, inst *Instance) {
	var body struct {
		To       string   `json:"to"`
		Text     string   `json:"text"`
		QuotedID string   `json:"quotedId"`
		Mentions []string `json:"mentions"`
	}
	if err := decode(r, &body); err != nil || body.To == "" || body.Text == "" {
		writeErr(w, 400, errors.New(`informe {"to": "...", "text": "..."}`))
		return
	}
	cli, ok := requireLogin(w, inst)
	if !ok {
		return
	}
	jid, err := resolveJID(r.Context(), cli, body.To)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	msg := &waE2E.Message{}
	if ci := contextInfo(body.QuotedID, body.Mentions); ci != nil {
		msg.ExtendedTextMessage = &waE2E.ExtendedTextMessage{Text: proto.String(body.Text), ContextInfo: ci}
	} else {
		msg.Conversation = proto.String(body.Text)
	}
	resp, err := cli.SendMessage(r.Context(), jid, msg)
	sendResult(w, resp, jid, err)
}

func (a *API) sendMedia(w http.ResponseWriter, r *http.Request, inst *Instance) {
	var body struct {
		To       string `json:"to"`
		Type     string `json:"type"` // image | video | audio | document | sticker
		URL      string `json:"url"`
		Base64   string `json:"base64"`
		Caption  string `json:"caption"`
		FileName string `json:"fileName"`
		Mimetype string `json:"mimetype"`
		PTT      bool   `json:"ptt"` // áudio como mensagem de voz
		QuotedID string `json:"quotedId"`
	}
	if err := decode(r, &body); err != nil || body.To == "" || (body.URL == "" && body.Base64 == "") {
		writeErr(w, 400, errors.New(`informe {"to", "type", "url" ou "base64"}`))
		return
	}
	cli, ok := requireLogin(w, inst)
	if !ok {
		return
	}
	jid, err := resolveJID(r.Context(), cli, body.To)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	data, err := loadMedia(r.Context(), body.URL, body.Base64)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	mime := body.Mimetype
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	if body.Type == "" {
		body.Type = guessType(mime)
	}
	if body.Type == "audio" && body.PTT && mime != "audio/ogg; codecs=opus" {
		mime = "audio/ogg; codecs=opus" // nota de voz precisa ser OGG/Opus
	}

	mediaType := map[string]whatsmeow.MediaType{
		"image": whatsmeow.MediaImage, "sticker": whatsmeow.MediaImage,
		"video": whatsmeow.MediaVideo, "audio": whatsmeow.MediaAudio,
		"document": whatsmeow.MediaDocument,
	}[body.Type]
	if mediaType == "" {
		writeErr(w, 400, errors.New("type deve ser image, video, audio, document ou sticker"))
		return
	}
	up, err := cli.Upload(r.Context(), data, mediaType)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("upload falhou: %w", err))
		return
	}
	size := proto.Uint64(uint64(len(data)))
	ci := contextInfo(body.QuotedID, nil)
	msg := &waE2E.Message{}
	switch body.Type {
	case "image":
		msg.ImageMessage = &waE2E.ImageMessage{Caption: proto.String(body.Caption), Mimetype: proto.String(mime),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: size, ContextInfo: ci}
	case "sticker":
		msg.StickerMessage = &waE2E.StickerMessage{Mimetype: proto.String(mime),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: size, ContextInfo: ci}
	case "video":
		msg.VideoMessage = &waE2E.VideoMessage{Caption: proto.String(body.Caption), Mimetype: proto.String(mime),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: size, ContextInfo: ci}
	case "audio":
		msg.AudioMessage = &waE2E.AudioMessage{Mimetype: proto.String(mime), PTT: proto.Bool(body.PTT),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: size, ContextInfo: ci}
	case "document":
		name := body.FileName
		if name == "" {
			name = "arquivo"
		}
		msg.DocumentMessage = &waE2E.DocumentMessage{Caption: proto.String(body.Caption), Mimetype: proto.String(mime),
			FileName: proto.String(name), Title: proto.String(name),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: size, ContextInfo: ci}
	}
	resp, err := cli.SendMessage(r.Context(), jid, msg)
	sendResult(w, resp, jid, err)
}

func guessType(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/webp"):
		return "sticker"
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	default:
		return "document"
	}
}

func loadMedia(ctx context.Context, url, b64 string) ([]byte, error) {
	if b64 != "" {
		if i := strings.Index(b64, ","); strings.HasPrefix(b64, "data:") && i > 0 {
			b64 = b64[i+1:]
		}
		return base64.StdEncoding.DecodeString(b64)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("erro ao baixar mídia: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("erro ao baixar mídia: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMediaBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMediaBytes {
		return nil, errors.New("mídia maior que 64 MB")
	}
	return data, nil
}

func (a *API) sendLocation(w http.ResponseWriter, r *http.Request, inst *Instance) {
	var body struct {
		To        string  `json:"to"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Name      string  `json:"name"`
		Address   string  `json:"address"`
	}
	if err := decode(r, &body); err != nil || body.To == "" {
		writeErr(w, 400, errors.New(`informe {"to", "latitude", "longitude"}`))
		return
	}
	cli, ok := requireLogin(w, inst)
	if !ok {
		return
	}
	jid, err := resolveJID(r.Context(), cli, body.To)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	resp, err := cli.SendMessage(r.Context(), jid, &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
		DegreesLatitude: proto.Float64(body.Latitude), DegreesLongitude: proto.Float64(body.Longitude),
		Name: proto.String(body.Name), Address: proto.String(body.Address),
	}})
	sendResult(w, resp, jid, err)
}

func (a *API) sendReaction(w http.ResponseWriter, r *http.Request, inst *Instance) {
	var body struct {
		To        string `json:"to"`        // chat onde está a mensagem
		MessageID string `json:"messageId"` // id da mensagem
		Emoji     string `json:"emoji"`     // "" remove a reação
		FromMe    bool   `json:"fromMe"`    // a mensagem foi enviada por você?
		Sender    string `json:"sender"`    // em grupos: quem enviou a mensagem
	}
	if err := decode(r, &body); err != nil || body.To == "" || body.MessageID == "" {
		writeErr(w, 400, errors.New(`informe {"to", "messageId", "emoji"}`))
		return
	}
	cli, ok := requireLogin(w, inst)
	if !ok {
		return
	}
	chat, err := resolveJID(r.Context(), cli, body.To)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	sender := chat
	if body.FromMe {
		sender = cli.Store.ID.ToNonAD()
	} else if body.Sender != "" {
		if sender, err = types.ParseJID(body.Sender); err != nil {
			writeErr(w, 400, err)
			return
		}
	}
	msg := cli.BuildReaction(chat, sender, body.MessageID, body.Emoji)
	resp, err := cli.SendMessage(r.Context(), chat, msg)
	sendResult(w, resp, chat, err)
}

func (a *API) checkNumbers(w http.ResponseWriter, r *http.Request, inst *Instance) {
	var body struct {
		Numbers []string `json:"numbers"`
	}
	if err := decode(r, &body); err != nil || len(body.Numbers) == 0 {
		writeErr(w, 400, errors.New(`informe {"numbers": ["5511999999999"]}`))
		return
	}
	cli, ok := requireLogin(w, inst)
	if !ok {
		return
	}
	nums := make([]string, len(body.Numbers))
	for i, n := range body.Numbers {
		nums[i] = "+" + strings.TrimPrefix(n, "+")
	}
	res, err := cli.IsOnWhatsApp(r.Context(), nums)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	out := make([]map[string]any, 0, len(res))
	for _, x := range res {
		out = append(out, map[string]any{"query": x.Query, "exists": x.IsIn, "jid": x.JID.String()})
	}
	writeJSON(w, 200, out)
}

func (a *API) listGroups(w http.ResponseWriter, r *http.Request, inst *Instance) {
	cli, ok := requireLogin(w, inst)
	if !ok {
		return
	}
	groups, err := cli.GetJoinedGroups(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		out = append(out, map[string]any{"jid": g.JID.String(), "name": g.Name, "participants": len(g.Participants)})
	}
	writeJSON(w, 200, out)
}

// ---------- disparo automático ----------

func (a *API) startCampaign(w http.ResponseWriter, r *http.Request, inst *Instance) {
	var body CampaignRequest
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	c, err := inst.StartCampaign(body)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, c.Snapshot())
}

func (a *API) getCampaign(w http.ResponseWriter, r *http.Request, inst *Instance) {
	c := inst.Campaign()
	if c == nil {
		writeJSON(w, 200, map[string]any{"status": "none"})
		return
	}
	writeJSON(w, 200, c.Snapshot())
}

func (a *API) stopCampaign(w http.ResponseWriter, r *http.Request, inst *Instance) {
	if err := inst.StopCampaign(); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]any{"stopping": true})
}

func jsonBody(v any) io.ReadCloser {
	b, _ := json.Marshal(v)
	return io.NopCloser(bytes.NewReader(b))
}

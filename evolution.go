package main

// Rotas compatíveis com o formato da Evolution API (usado também pela WaDuck),
// para sistemas que já falavam com essas APIs só trocarem URL e chave.
// Funcionam com e sem o prefixo /v1. Autenticação pelo cabeçalho "apikey".

import (
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func (a *API) registerEvolution(mux *http.ServeMux) {
	for _, p := range []string{"", "/v1"} {
		mux.HandleFunc("POST "+p+"/message/sendText/{name}", a.auth(a.evoInstance(a.evoSendText)))
		mux.HandleFunc("POST "+p+"/message/sendMedia/{name}", a.auth(a.evoInstance(a.evoSendMedia)))
		mux.HandleFunc("POST "+p+"/instance/create", a.auth(a.evoCreate))
		mux.HandleFunc("GET "+p+"/instance/fetchInstances", a.auth(a.evoFetch))
		mux.HandleFunc("GET "+p+"/instance/connect/{name}", a.auth(a.evoInstance(a.evoConnect)))
		mux.HandleFunc("GET "+p+"/instance/connectionState/{name}", a.auth(a.evoInstance(a.evoState)))
		mux.HandleFunc("DELETE "+p+"/instance/logout/{name}", a.auth(a.evoInstance(a.evoLogout)))
		mux.HandleFunc("DELETE "+p+"/instance/delete/{name}", a.auth(a.evoDelete))
		mux.HandleFunc("POST "+p+"/webhook/set/{name}", a.auth(a.evoInstance(a.evoWebhook)))
	}
}

func evoErr(w http.ResponseWriter, code int, msgs ...any) {
	writeJSON(w, code, map[string]any{
		"status":   code,
		"error":    http.StatusText(code),
		"response": map[string]any{"message": msgs},
	})
}

func (a *API) evoInstance(next instHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		inst, err := a.mgr.Get(name)
		if err != nil {
			// Nome desconhecido (ex.: instância antiga da WaDuck salva no site):
			// usa FALLBACK_INSTANCE ou, se só houver uma instância conectada, ela.
			if fb := a.mgr.Fallback(); fb != nil {
				log.Printf("[evolution] %s %s: instância %q não existe, usando %q", r.Method, r.URL.Path, name, fb.Name)
				inst = fb
			} else {
				log.Printf("[evolution] %s %s: instância %q não existe", r.Method, r.URL.Path, name)
				evoErr(w, http.StatusNotFound, `The "`+name+`" instance does not exist`)
				return
			}
		}
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next(rec, r, inst)
		log.Printf("[evolution] %s %s -> instância %q, resposta %d", r.Method, r.URL.Path, inst.Name, rec.code)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

// evoState traduz nosso status para o da Evolution: open | connecting | close.
func evoStateName(st InstanceStatus) string {
	switch st.Status {
	case "connected":
		return "open"
	case "qr", "connecting":
		return "connecting"
	default:
		return "close"
	}
}

func (a *API) evoSendText(w http.ResponseWriter, r *http.Request, inst *Instance) {
	var body struct {
		Number string `json:"number"`
		Text   string `json:"text"`
		Delay  int    `json:"delay"` // ms, opcional
		// formato antigo da Evolution v1
		TextMessage *struct {
			Text string `json:"text"`
		} `json:"textMessage"`
	}
	if err := decode(r, &body); err != nil {
		evoErr(w, 400, err.Error())
		return
	}
	if body.Text == "" && body.TextMessage != nil {
		body.Text = body.TextMessage.Text
	}
	if body.Number == "" || body.Text == "" {
		evoErr(w, 400, "number e text são obrigatórios")
		return
	}
	cli := inst.Client()
	if !cli.IsLoggedIn() {
		evoErr(w, 500, "Connection Closed")
		return
	}
	jid, err := resolveJID(r.Context(), cli, body.Number)
	if err != nil {
		log.Printf("[evolution] número %s: %v", body.Number, err)
		evoErr(w, 400, map[string]any{"exists": false, "number": body.Number, "error": err.Error()})
		return
	}
	if body.Delay > 0 && body.Delay <= 60000 {
		time.Sleep(time.Duration(body.Delay) * time.Millisecond)
	}
	resp, err := cli.SendMessage(r.Context(), jid, &waE2E.Message{Conversation: proto.String(body.Text)})
	if err != nil {
		evoErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"key":              map[string]any{"remoteJid": jid.String(), "fromMe": true, "id": resp.ID},
		"message":          map[string]any{"conversation": body.Text},
		"messageTimestamp": resp.Timestamp.Unix(),
		"status":           "PENDING",
	})
}

func (a *API) evoSendMedia(w http.ResponseWriter, r *http.Request, inst *Instance) {
	var body struct {
		Number    string `json:"number"`
		Mediatype string `json:"mediatype"`
		Mimetype  string `json:"mimetype"`
		Caption   string `json:"caption"`
		Media     string `json:"media"` // URL ou base64
		FileName  string `json:"fileName"`
	}
	if err := decode(r, &body); err != nil {
		evoErr(w, 400, err.Error())
		return
	}
	// Reaproveita o handler nativo montando a requisição equivalente.
	native := map[string]any{
		"to": body.Number, "type": body.Mediatype, "caption": body.Caption,
		"fileName": body.FileName, "mimetype": body.Mimetype,
	}
	if strings.HasPrefix(body.Media, "http://") || strings.HasPrefix(body.Media, "https://") {
		native["url"] = body.Media
	} else {
		native["base64"] = body.Media
	}
	if !inst.Client().IsLoggedIn() {
		evoErr(w, 500, "Connection Closed")
		return
	}
	r2 := r.Clone(r.Context())
	r2.Body = jsonBody(native)
	a.sendMedia(w, r2, inst)
}

func (a *API) evoCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InstanceName string `json:"instanceName"`
		Qrcode       bool   `json:"qrcode"`
		Webhook      any    `json:"webhook"`
	}
	if err := decode(r, &body); err != nil {
		evoErr(w, 400, err.Error())
		return
	}
	webhook := ""
	switch v := body.Webhook.(type) {
	case string:
		webhook = v
	case map[string]any:
		if u, ok := v["url"].(string); ok {
			webhook = u
		}
	}
	inst, err := a.mgr.Create(r.Context(), body.InstanceName, webhook)
	if errors.Is(err, ErrExists) {
		evoErr(w, 403, `This name "`+body.InstanceName+`" is already in use.`)
		return
	} else if err != nil {
		evoErr(w, 400, err.Error())
		return
	}
	out := map[string]any{
		"instance": map[string]any{"instanceName": inst.Name, "instanceId": inst.Name, "status": "created"},
		"hash":     map[string]any{"apikey": a.cfg.APIToken},
	}
	if body.Qrcode {
		if err := inst.Connect(); err == nil {
			out["qrcode"] = evoQR(inst)
		}
	}
	writeJSON(w, 201, out)
}

func evoQR(inst *Instance) map[string]any {
	qr := inst.QR(8 * time.Second)
	if qr == "" {
		return map[string]any{"count": 0}
	}
	png, err := qrcode.Encode(qr, qrcode.Medium, 512)
	if err != nil {
		return map[string]any{"code": qr, "count": 1}
	}
	return map[string]any{
		"pairingCode": nil,
		"code":        qr,
		"base64":      "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
		"count":       1,
	}
}

func (a *API) evoConnect(w http.ResponseWriter, r *http.Request, inst *Instance) {
	if inst.Client().IsLoggedIn() {
		writeJSON(w, 200, map[string]any{"instance": map[string]any{"instanceName": inst.Name, "state": "open"}})
		return
	}
	if num := r.URL.Query().Get("number"); num != "" {
		code, err := inst.PairCode(r.Context(), num)
		if err != nil {
			evoErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"pairingCode": code, "code": nil, "count": 1})
		return
	}
	if err := inst.Connect(); err != nil {
		evoErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, evoQR(inst))
}

func (a *API) evoState(w http.ResponseWriter, r *http.Request, inst *Instance) {
	writeJSON(w, 200, map[string]any{"instance": map[string]any{"instanceName": inst.Name, "state": evoStateName(inst.Status())}})
}

func (a *API) evoFetch(w http.ResponseWriter, r *http.Request) {
	list := a.mgr.List()
	out := make([]map[string]any, 0, len(list))
	for _, st := range list {
		out = append(out, map[string]any{
			"name": st.Name, "id": st.Name, "connectionStatus": evoStateName(st),
			"ownerJid": st.JID, "profileName": st.PushName, "number": st.Phone,
			"instance": map[string]any{"instanceName": st.Name, "status": evoStateName(st), "owner": st.JID, "profileName": st.PushName},
		})
	}
	writeJSON(w, 200, out)
}

func (a *API) evoLogout(w http.ResponseWriter, r *http.Request, inst *Instance) {
	if err := inst.Logout(r.Context()); err != nil {
		evoErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"status": "SUCCESS", "error": false, "response": map[string]any{"message": "Instance logged out"}})
}

func (a *API) evoDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.mgr.Delete(r.Context(), r.PathValue("name")); err != nil {
		code := 500
		if errors.Is(err, ErrNotFound) {
			code = 404
		}
		evoErr(w, code, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"status": "SUCCESS", "error": false, "response": map[string]any{"message": "Instance deleted"}})
}

func (a *API) evoWebhook(w http.ResponseWriter, r *http.Request, inst *Instance) {
	var body struct {
		URL     string `json:"url"`
		Enabled *bool  `json:"enabled"`
		Webhook *struct {
			URL     string `json:"url"`
			Enabled *bool  `json:"enabled"`
		} `json:"webhook"`
	}
	if err := decode(r, &body); err != nil {
		evoErr(w, 400, err.Error())
		return
	}
	url, enabled := body.URL, body.Enabled
	if body.Webhook != nil {
		url, enabled = body.Webhook.URL, body.Webhook.Enabled
	}
	if enabled != nil && !*enabled {
		url = ""
	}
	if err := inst.SetWebhook(r.Context(), url); err != nil {
		evoErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"webhook": map[string]any{"instanceName": inst.Name, "url": url, "enabled": url != ""}})
}

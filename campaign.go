package main

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// Campaign é um disparo automático de texto para uma lista de números,
// com intervalo entre cada mensagem.
type Campaign struct {
	mu       *sync.RWMutex
	cancel   context.CancelFunc
	Status   string           `json:"status"` // running | waiting_connection | finished | stopped
	Message  string           `json:"message"`
	Delay    int              `json:"delaySeconds"`
	Jitter   int              `json:"jitterSeconds"`
	Total    int              `json:"total"`
	Sent     int              `json:"sent"`
	Failed   int              `json:"failed"`
	NextAt   int64            `json:"nextAt,omitempty"` // unix: quando sai a próxima
	Started  int64            `json:"startedAt"`
	Finished int64            `json:"finishedAt,omitempty"`
	Items    []CampaignTarget `json:"items"`
}

type CampaignTarget struct {
	Phone  string `json:"phone"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"` // pending | sent | failed
	Error  string `json:"error,omitempty"`
	ID     string `json:"id,omitempty"`
	At     int64  `json:"at,omitempty"`
}

type CampaignRequest struct {
	Numbers       []string `json:"numbers"` // "5511999999999" ou "5511999999999;Nome"
	Text          string   `json:"text"`    // aceita {nome}
	DelaySeconds  int      `json:"delaySeconds"`
	JitterSeconds *int     `json:"jitterSeconds"`
}

// ParseTargets aceita uma lista em que cada item pode ser "numero" ou "numero;nome"
// (também aceita vírgula ou tab). Remove repetidos e linhas vazias.
func ParseTargets(lines []string) []CampaignTarget {
	seen := map[string]bool{}
	var out []CampaignTarget
	for _, raw := range lines {
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			numPart, name := line, ""
			for _, sep := range []string{";", "\t", ","} {
				if a, b, ok := strings.Cut(line, sep); ok {
					numPart, name = a, strings.TrimSpace(b)
					break
				}
			}
			digits := strings.Map(func(r rune) rune {
				if r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, numPart)
			if len(digits) < 8 || seen[digits] {
				continue
			}
			seen[digits] = true
			out = append(out, CampaignTarget{Phone: digits, Name: name, Status: "pending"})
		}
	}
	return out
}

func (c *Campaign) Snapshot() Campaign {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cp := Campaign{
		Status: c.Status, Message: c.Message, Delay: c.Delay, Jitter: c.Jitter,
		Total: c.Total, Sent: c.Sent, Failed: c.Failed, NextAt: c.NextAt,
		Started: c.Started, Finished: c.Finished,
	}
	cp.Items = append([]CampaignTarget(nil), c.Items...)
	return cp
}

func (i *Instance) Campaign() *Campaign {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.campaign
}

func (i *Instance) StartCampaign(req CampaignRequest) (*Campaign, error) {
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		return nil, errors.New("escreva a mensagem")
	}
	targets := ParseTargets(req.Numbers)
	if len(targets) == 0 {
		return nil, errors.New("nenhum número válido na lista")
	}
	if req.DelaySeconds <= 0 {
		req.DelaySeconds = 60
	}
	if req.DelaySeconds < 5 {
		req.DelaySeconds = 5
	}
	jitter := req.DelaySeconds / 4 // padrão: variação de 25%
	if req.JitterSeconds != nil {
		jitter = max(0, *req.JitterSeconds)
	}
	if jitter >= req.DelaySeconds {
		jitter = req.DelaySeconds - 1
	}

	i.mu.Lock()
	if i.campaign != nil {
		st := i.campaign.Snapshot().Status
		if st == "running" || st == "waiting_connection" {
			i.mu.Unlock()
			return nil, errors.New("já existe um disparo em andamento nesta instância; pare antes de iniciar outro")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Campaign{
		mu: &sync.RWMutex{}, cancel: cancel, Status: "running", Message: req.Text,
		Delay: req.DelaySeconds, Jitter: jitter, Total: len(targets),
		Started: time.Now().Unix(), Items: targets,
	}
	i.campaign = c
	i.mu.Unlock()

	log.Printf("[%s] disparo iniciado: %d números, intervalo %ds (±%ds)", i.Name, len(targets), req.DelaySeconds, jitter)
	go i.runCampaign(ctx, c)
	return c, nil
}

func (i *Instance) StopCampaign() error {
	c := i.Campaign()
	if c == nil {
		return errors.New("nenhum disparo nesta instância")
	}
	c.cancel()
	return nil
}

func (i *Instance) runCampaign(ctx context.Context, c *Campaign) {
	setStatus := func(s string) {
		c.mu.Lock()
		c.Status = s
		if s == "finished" || s == "stopped" {
			c.Finished = time.Now().Unix()
			c.NextAt = 0
		}
		c.mu.Unlock()
	}
	sleep := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}

	for idx := range c.Items {
		// Espera o número estar conectado (não gasta a lista se a conexão cair).
		for !i.Client().IsLoggedIn() {
			setStatus("waiting_connection")
			if !sleep(5 * time.Second) {
				setStatus("stopped")
				log.Printf("[%s] disparo parado", i.Name)
				return
			}
		}
		setStatus("running")
		if ctx.Err() != nil {
			setStatus("stopped")
			log.Printf("[%s] disparo parado", i.Name)
			return
		}

		c.mu.RLock()
		t := c.Items[idx]
		c.mu.RUnlock()
		text := strings.NewReplacer("{nome}", firstNonEmpty(t.Name, ""), "{numero}", t.Phone).Replace(c.Message)

		id, err := i.sendSimpleText(ctx, t.Phone, text)
		c.mu.Lock()
		c.Items[idx].At = time.Now().Unix()
		if err != nil {
			c.Items[idx].Status, c.Items[idx].Error = "failed", err.Error()
			c.Failed++
		} else {
			c.Items[idx].Status, c.Items[idx].ID = "sent", id
			c.Sent++
		}
		c.mu.Unlock()
		if err != nil {
			log.Printf("[%s] disparo %d/%d para %s: FALHOU (%v)", i.Name, idx+1, c.Total, t.Phone, err)
		} else {
			log.Printf("[%s] disparo %d/%d para %s: enviado", i.Name, idx+1, c.Total, t.Phone)
		}

		if idx == len(c.Items)-1 {
			break
		}
		wait := time.Duration(c.Delay) * time.Second
		if c.Jitter > 0 {
			wait += time.Duration(rand.IntN(2*c.Jitter+1)-c.Jitter) * time.Second
		}
		c.mu.Lock()
		c.NextAt = time.Now().Add(wait).Unix()
		c.mu.Unlock()
		if !sleep(wait) {
			setStatus("stopped")
			log.Printf("[%s] disparo parado", i.Name)
			return
		}
	}
	setStatus("finished")
	snap := c.Snapshot()
	log.Printf("[%s] disparo concluído: %d enviadas, %d falharam", i.Name, snap.Sent, snap.Failed)
	i.emit("campaign", map[string]any{"status": "finished", "sent": snap.Sent, "failed": snap.Failed, "total": snap.Total})
}

func (i *Instance) sendSimpleText(ctx context.Context, phone, text string) (string, error) {
	cli := i.Client()
	sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	jid, err := resolveJID(sctx, cli, phone)
	if err != nil {
		return "", err
	}
	resp, err := cli.SendMessage(sctx, jid, &waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

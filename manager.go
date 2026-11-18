package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var (
	ErrNotFound     = errors.New("instância não encontrada")
	ErrExists       = errors.New("instância já existe")
	ErrNotLoggedIn  = errors.New("instância não está conectada ao WhatsApp")
	ErrInvalidName  = errors.New("nome inválido: use letras, números, ponto, - ou _ (até 100 caracteres)")
	validNameRegexp = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,100}$`)
)

// Instance é um número de WhatsApp conectado (ou aguardando conexão).
type Instance struct {
	Name     string
	mgr      *Manager
	mu       sync.RWMutex
	webhook  string
	client   *whatsmeow.Client
	qr       string
	qrAt     time.Time
	token    string
	pairing  bool
	lastErr  string
	campaign *Campaign
}

type InstanceStatus struct {
	Name      string `json:"name"`
	Status    string `json:"status"` // disconnected | connecting | qr | connected
	LoggedIn  bool   `json:"loggedIn"`
	Connected bool   `json:"connected"`
	JID       string `json:"jid,omitempty"`
	Phone     string `json:"phone,omitempty"`
	PushName  string `json:"pushName,omitempty"`
	Webhook   string `json:"webhook,omitempty"`
	Token     string `json:"token"`
	LastError string `json:"lastError,omitempty"`
}

type Manager struct {
	cfg       Config
	db        *sql.DB
	container *sqlstore.Container
	waLog     waLog.Logger
	mu        sync.RWMutex
	instances map[string]*Instance
	webhooks  *WebhookSender
}

func NewManager(ctx context.Context, cfg Config) (*Manager, error) {
	db, err := sql.Open(cfg.DBDialect, cfg.DBURL)
	if err != nil {
		return nil, err
	}
	if cfg.DBDialect == "sqlite" {
		db.SetMaxOpenConns(1)
	}
	if err = db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("banco de dados: %w", err)
	}
	store.DeviceProps.Os = proto.String("WhatsAPI")

	logger := waLog.Stdout("whatsmeow", cfg.LogLevel, true)
	container := sqlstore.NewWithDB(db, cfg.DBDialect, logger.Sub("DB"))
	if err = container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("migração whatsmeow: %w", err)
	}
	if _, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS api_instances (
		name       TEXT PRIMARY KEY,
		jid        TEXT NOT NULL DEFAULT '',
		webhook    TEXT NOT NULL DEFAULT '',
		created_at BIGINT NOT NULL
	)`); err != nil {
		return nil, fmt.Errorf("migração api_instances: %w", err)
	}
	// Coluna nova (versões antigas não têm): ignora o erro se já existir.
	_, _ = db.ExecContext(ctx, `ALTER TABLE api_instances ADD COLUMN token TEXT NOT NULL DEFAULT ''`)

	m := &Manager{
		cfg:       cfg,
		db:        db,
		container: container,
		waLog:     logger,
		instances: map[string]*Instance{},
		webhooks:  NewWebhookSender(cfg.WebhookSecret),
	}
	if err = m.loadAll(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) loadAll(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx, `SELECT name, jid, webhook, token FROM api_instances ORDER BY created_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type row struct{ name, jid, webhook, token string }
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.jid, &r.webhook, &r.token); err != nil {
			return err
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range list {
		inst := &Instance{Name: r.name, mgr: m, webhook: r.webhook, token: r.token}
		if inst.token == "" { // instâncias criadas antes das chaves por instância
			inst.token = newInstanceToken()
			_, _ = m.db.ExecContext(ctx, `UPDATE api_instances SET token = $1 WHERE name = $2`, inst.token, r.name)
		}
		var device *store.Device
		if r.jid != "" {
			if jid, err := types.ParseJID(r.jid); err == nil {
				device, _ = m.container.GetDevice(ctx, jid)
			}
		}
		if device == nil {
			device = m.container.NewDevice()
		}
		inst.setClient(device)
		m.instances[r.name] = inst
		if device.ID != nil {
			go func() {
				if err := inst.client.Connect(); err != nil {
					inst.setErr(err)
					log.Printf("[%s] falha ao reconectar: %v", inst.Name, err)
				}
			}()
		}
	}
	log.Printf("%d instância(s) carregada(s)", len(list))
	return nil
}

func (m *Manager) Create(ctx context.Context, name, webhook string) (*Instance, error) {
	if !validNameRegexp.MatchString(name) {
		return nil, ErrInvalidName
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.instances[name]; ok {
		return nil, ErrExists
	}
	token := newInstanceToken()
	if _, err := m.db.ExecContext(ctx,
		`INSERT INTO api_instances (name, jid, webhook, token, created_at) VALUES ($1, '', $2, $3, $4)`,
		name, webhook, token, time.Now().Unix()); err != nil {
		return nil, err
	}
	inst := &Instance{Name: name, mgr: m, webhook: webhook, token: token}
	inst.setClient(m.container.NewDevice())
	m.instances[name] = inst
	return inst, nil
}

func (m *Manager) Get(name string) (*Instance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst, ok := m.instances[name]
	if !ok {
		return nil, ErrNotFound
	}
	return inst, nil
}

// ByToken acha a instância dona de uma chave.
func (m *Manager) ByToken(tok string) *Instance {
	if tok == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, inst := range m.instances {
		if subtle.ConstantTimeCompare([]byte(inst.Token()), []byte(tok)) == 1 {
			return inst
		}
	}
	return nil
}

func newInstanceToken() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return "inst_" + hex.EncodeToString(b)
}

// Fallback devolve a instância padrão para nomes desconhecidos:
// FALLBACK_INSTANCE (se existir) ou a única instância conectada.
func (m *Manager) Fallback() *Instance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if name := m.cfg.FallbackInstance; name != "" {
		if inst, ok := m.instances[name]; ok {
			return inst
		}
	}
	var only *Instance
	for _, inst := range m.instances {
		if inst.Client().IsLoggedIn() {
			if only != nil {
				return nil // mais de uma conectada: não dá pra adivinhar
			}
			only = inst
		}
	}
	return only
}

func (m *Manager) List() []InstanceStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]InstanceStatus, 0, len(m.instances))
	for _, inst := range m.instances {
		out = append(out, inst.Status())
	}
	return out
}

func (m *Manager) Delete(ctx context.Context, name string) error {
	inst, err := m.Get(name)
	if err != nil {
		return err
	}
	cli := inst.Client()
	if cli.Store.ID != nil {
		if err := cli.Logout(ctx); err != nil {
			// Se não der para deslogar no servidor, apaga localmente mesmo assim.
			_ = cli.Store.Delete(ctx)
		}
	}
	cli.Disconnect()
	m.mu.Lock()
	delete(m.instances, name)
	m.mu.Unlock()
	_, err = m.db.ExecContext(ctx, `DELETE FROM api_instances WHERE name = $1`, name)
	return err
}

func (m *Manager) Close() {
	m.mu.RLock()
	for _, inst := range m.instances {
		inst.Client().Disconnect()
	}
	m.mu.RUnlock()
	_ = m.db.Close()
}

// ---------- Instance ----------

func (i *Instance) Client() *whatsmeow.Client {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.client
}

func (i *Instance) Token() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.token
}

// RotateToken gera uma chave nova; a antiga para de funcionar na hora.
func (i *Instance) RotateToken(ctx context.Context) (string, error) {
	tok := newInstanceToken()
	if _, err := i.mgr.db.ExecContext(ctx, `UPDATE api_instances SET token = $1 WHERE name = $2`, tok, i.Name); err != nil {
		return "", err
	}
	i.mu.Lock()
	i.token = tok
	i.mu.Unlock()
	return tok, nil
}

func (i *Instance) Webhook() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.webhook
}

func (i *Instance) setErr(err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err == nil {
		i.lastErr = ""
	} else {
		i.lastErr = err.Error()
	}
}

func (i *Instance) setClient(device *store.Device) {
	cli := whatsmeow.NewClient(device, i.mgr.waLog.Sub(i.Name))
	cli.EnableAutoReconnect = true
	cli.AddEventHandler(i.handleEvent)
	i.mu.Lock()
	i.client = cli
	i.mu.Unlock()
}

func (i *Instance) SetWebhook(ctx context.Context, url string) error {
	if _, err := i.mgr.db.ExecContext(ctx, `UPDATE api_instances SET webhook = $1 WHERE name = $2`, url, i.Name); err != nil {
		return err
	}
	i.mu.Lock()
	i.webhook = url
	i.mu.Unlock()
	return nil
}

func (i *Instance) saveJID(jid string) {
	if _, err := i.mgr.db.Exec(`UPDATE api_instances SET jid = $1 WHERE name = $2`, jid, i.Name); err != nil {
		log.Printf("[%s] erro ao salvar jid: %v", i.Name, err)
	}
}

func (i *Instance) Status() InstanceStatus {
	cli := i.Client()
	i.mu.RLock()
	defer i.mu.RUnlock()
	st := InstanceStatus{
		Name:      i.Name,
		LoggedIn:  cli.IsLoggedIn(),
		Connected: cli.IsConnected(),
		Webhook:   i.webhook,
		Token:     i.token,
		LastError: i.lastErr,
		PushName:  cli.Store.PushName,
	}
	if cli.Store.ID != nil {
		st.JID = cli.Store.ID.ToNonAD().String()
		st.Phone = cli.Store.ID.User
	}
	switch {
	case st.LoggedIn:
		st.Status = "connected"
	case i.qr != "":
		st.Status = "qr"
	case i.pairing || st.Connected:
		st.Status = "connecting"
	default:
		st.Status = "disconnected"
	}
	return st
}

// Connect inicia a conexão. Se o número ainda não foi pareado, gera QR Codes.
func (i *Instance) Connect() error {
	cli := i.Client()
	if cli.IsConnected() {
		return nil
	}
	if cli.Store.ID != nil {
		return cli.Connect()
	}
	log.Printf("[%s] conectando ao WhatsApp para gerar o QR...", i.Name)
	qrChan, err := cli.GetQRChannel(context.Background())
	if err != nil {
		log.Printf("[%s] erro ao preparar QR: %v", i.Name, err)
		return err
	}
	i.mu.Lock()
	i.pairing = true
	i.lastErr = ""
	i.mu.Unlock()
	if err := cli.Connect(); err != nil {
		log.Printf("[%s] erro ao conectar: %v", i.Name, err)
		i.mu.Lock()
		i.pairing = false
		i.lastErr = err.Error()
		i.mu.Unlock()
		return err
	}
	go func() {
		for evt := range qrChan {
			i.mu.Lock()
			switch evt.Event {
			case whatsmeow.QRChannelEventCode:
				i.qr = evt.Code
				i.qrAt = time.Now()
				log.Printf("[%s] QR Code pronto, abra o painel para escanear", i.Name)
			case "success":
				i.qr, i.pairing = "", false
				log.Printf("[%s] QR escaneado com sucesso", i.Name)
			default: // timeout, erro etc.
				log.Printf("[%s] pareamento encerrado: %s %v", i.Name, evt.Event, evt.Error)
				i.qr, i.pairing = "", false
				if evt.Error != nil {
					i.lastErr = evt.Error.Error()
				} else {
					i.lastErr = "pareamento encerrado: " + evt.Event
				}
			}
			i.mu.Unlock()
		}
		// Só derruba a conexão se NÃO pareou. Depois do pareamento o WhatsApp
		// fecha e reabre a conexão sozinho (erro 515), e não podemos atrapalhar isso.
		if cli.Store.ID == nil {
			cli.Disconnect()
		} else if !cli.IsConnected() {
			time.Sleep(3 * time.Second)
			if !cli.IsConnected() {
				log.Printf("[%s] reconectando após o pareamento...", i.Name)
				_ = cli.Connect()
			}
		}
	}()
	return nil
}

// QR retorna o QR Code atual (string), esperando até `wait` para ele aparecer.
func (i *Instance) QR(wait time.Duration) string {
	deadline := time.Now().Add(wait)
	for {
		i.mu.RLock()
		qr, pairing := i.qr, i.pairing
		i.mu.RUnlock()
		if qr != "" || !pairing || time.Now().After(deadline) {
			return qr
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// PairCode pareia via código de 8 dígitos (alternativa ao QR Code).
func (i *Instance) PairCode(ctx context.Context, phone string) (string, error) {
	cli := i.Client()
	if cli.Store.ID != nil {
		return "", errors.New("instância já está pareada")
	}
	if !cli.IsConnected() {
		if err := i.Connect(); err != nil {
			return "", err
		}
		// Espera o primeiro QR para garantir que o websocket está pronto.
		i.QR(15 * time.Second)
	}
	return cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
}

func (i *Instance) Logout(ctx context.Context) error {
	cli := i.Client()
	if cli.Store.ID == nil {
		cli.Disconnect()
		return nil
	}
	err := cli.Logout(ctx)
	cli.Disconnect()
	i.resetDevice()
	return err
}

// resetDevice troca por um device novo para permitir parear outro número.
func (i *Instance) resetDevice() {
	i.saveJID("")
	i.setClient(i.mgr.container.NewDevice())
	i.mu.Lock()
	i.qr, i.pairing = "", false
	i.mu.Unlock()
}

func (i *Instance) handleEvent(rawEvt any) {
	switch evt := rawEvt.(type) {
	case *events.PairSuccess:
		log.Printf("[%s] pareado com %s", i.Name, evt.ID)
		i.saveJID(evt.ID.String())
		i.emit("connection", map[string]any{"status": "paired", "jid": evt.ID.ToNonAD().String()})
	case *events.Connected:
		i.setErr(nil)
		log.Printf("[%s] conectado", i.Name)
		i.emit("connection", map[string]any{"status": "connected"})
	case *events.Disconnected:
		i.emit("connection", map[string]any{"status": "disconnected"})
	case *events.LoggedOut:
		log.Printf("[%s] deslogado pelo celular (motivo %v)", i.Name, evt.Reason)
		i.emit("connection", map[string]any{"status": "logged_out", "reason": evt.Reason.String()})
		go i.resetDevice()
	case *events.StreamReplaced:
		i.setErr(errors.New("sessão aberta em outro lugar (stream replaced)"))
	case *events.Message:
		go i.emit("message", i.messagePayload(evt))
	case *events.Receipt:
		i.emit("receipt", map[string]any{
			"chat":      evt.Chat.String(),
			"sender":    evt.Sender.ToNonAD().String(),
			"ids":       evt.MessageIDs,
			"type":      receiptName(evt.Type),
			"timestamp": evt.Timestamp.Unix(),
		})
	}
}

func receiptName(t types.ReceiptType) string {
	switch t {
	case types.ReceiptTypeDelivered:
		return "delivered"
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf:
		return "read"
	case types.ReceiptTypePlayed, types.ReceiptTypePlayedSelf:
		return "played"
	default:
		return string(t)
	}
}

func (i *Instance) emit(event string, data map[string]any) {
	url := i.Webhook()
	if url == "" {
		return
	}
	i.mgr.webhooks.Send(url, map[string]any{
		"instance":  i.Name,
		"event":     event,
		"timestamp": time.Now().Unix(),
		"data":      data,
	})
}

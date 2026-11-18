package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func main() {
	// Trabalha sempre na pasta do executável: o .env e a pasta data ficam ao lado dele,
	// não importa de onde ele foi aberto (atalho, terminal, etc.).
	if exe, err := os.Executable(); err == nil && !strings.Contains(exe, "go-build") {
		_ = os.Chdir(filepath.Dir(exe))
	}
	loadDotEnv(".env")
	ensureToken(".env")
	cfg := Config{
		Port:          env("PORT", "8080"),
		APIToken:      env("API_TOKEN", ""),
		DBDialect:     env("DB_DIALECT", "sqlite"),
		DBURL:         env("DB_URL", "file:data/whatsapi.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"),
		WebhookSecret: env("WEBHOOK_SECRET", ""),
		MediaBase64:   env("WEBHOOK_MEDIA_BASE64", "false") == "true",
		LogLevel:      env("LOG_LEVEL", "INFO"),
	}
	if cfg.APIToken == "" {
		log.Println("defina a variável de ambiente API_TOKEN")
		fatalPause()
	}
	if strings.HasPrefix(cfg.DBDialect, "sqlite") {
		cfg.DBDialect = "sqlite" // driver em Go puro (modernc.org/sqlite)
		_ = os.MkdirAll("data", 0o755)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr, err := NewManager(ctx, cfg)
	if err != nil {
		log.Printf("erro ao iniciar: %v", err)
		fatalPause()
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           NewRouter(mgr, cfg),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Printf("ERRO: a porta %s já está em uso. Provavelmente o WhatsAPI já está aberto em outra janela.", cfg.Port)
		log.Printf("Feche as outras janelas do WhatsAPI (ou finalize whatsapi.exe no Gerenciador de Tarefas) e abra de novo.")
		fatalPause()
	}
	go func() {
		wd, _ := os.Getwd()
		log.Printf("Pasta: %s", wd)
		log.Printf("Token da API: %s", cfg.APIToken)
		log.Printf("WhatsAPI rodando! Abra o painel em http://localhost:%s", cfg.Port)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Println(err)
			fatalPause()
		}
	}()

	<-ctx.Done()
	log.Println("encerrando...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	mgr.Close()
}

type Config struct {
	Port          string
	APIToken      string
	DBDialect     string
	DBURL         string
	WebhookSecret string
	MediaBase64   bool
	LogLevel      string
}

// loadDotEnv lê um arquivo .env simples (CHAVE=valor) sem sobrescrever variáveis já definidas.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimPrefix(strings.TrimSpace(k), "\ufeff") // BOM do Bloco de Notas
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}

// ensureToken gera um API_TOKEN na primeira execução e salva no .env.
func ensureToken(path string) {
	if tok := env("API_TOKEN", ""); tok != "" {
		_ = os.Setenv("API_TOKEN", tok) // sem espaços sobrando
		return
	}
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	_ = os.Setenv("API_TOKEN", tok)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		fmt.Fprintf(f, "\nAPI_TOKEN=%s\n", tok)
		f.Close()
	}
	log.Printf("Token novo gerado e salvo em %s", path)
}

// fatalPause mantém a janela aberta no Windows para a pessoa conseguir ler o erro.
func fatalPause() {
	fmt.Println("\nPressione Enter para fechar...")
	_, _ = fmt.Scanln()
	os.Exit(1)
}

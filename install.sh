#!/usr/bin/env bash
# Instalador do WhatsAPI para Ubuntu (Oracle Cloud, VPS etc.)
# Uso: curl -fsSL https://raw.githubusercontent.com/robsonsjb67-cpu/whatsapi/main/install.sh | sudo bash
# Com domínio próprio: ... | sudo DOMAIN=api.seudominio.com.br bash
set -euo pipefail

REPO_RAW="${REPO_RAW:-https://raw.githubusercontent.com/robsonsjb67-cpu/whatsapi/main}"
DIR=/opt/whatsapi

if [ "$(id -u)" != "0" ]; then echo "Rode com sudo."; exit 1; fi

echo "==> Detectando o servidor..."
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "Arquitetura não suportada: $(uname -m)"; exit 1 ;;
esac

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl ca-certificates gnupg debian-keyring debian-archive-keyring apt-transport-https iptables-persistent >/dev/null 2>&1 || \
  apt-get install -y -qq curl ca-certificates gnupg apt-transport-https >/dev/null

PUBLIC_IP="$(curl -fsS4 https://api.ipify.org || curl -fsS4 https://ifconfig.me)"
if [ -z "${DOMAIN:-}" ]; then
  # Endereço grátis que aponta para o IP do servidor, sem precisar mexer em DNS.
  DOMAIN="api.$(echo "$PUBLIC_IP" | tr . -).sslip.io"
fi

echo "==> Baixando o WhatsAPI ($ARCH)..."
mkdir -p "$DIR/data"
curl -fsSL "$REPO_RAW/bin/whatsapi-linux-$ARCH.gz" | gunzip > "$DIR/whatsapi.new"
chmod +x "$DIR/whatsapi.new"
mv "$DIR/whatsapi.new" "$DIR/whatsapi"

if [ ! -f "$DIR/.env" ] || ! grep -q '^API_TOKEN=.\+' "$DIR/.env"; then
  TOKEN="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  cat > "$DIR/.env" <<ENV
API_TOKEN=$TOKEN
PORT=8080
LOG_LEVEL=INFO
ENV
fi
chmod 600 "$DIR/.env"
TOKEN="$(grep '^API_TOKEN=' "$DIR/.env" | cut -d= -f2)"

id whatsapi >/dev/null 2>&1 || useradd --system --home "$DIR" --shell /usr/sbin/nologin whatsapi
chown -R whatsapi:whatsapi "$DIR"

cat > /etc/systemd/system/whatsapi.service <<UNIT
[Unit]
Description=WhatsAPI
After=network-online.target
Wants=network-online.target

[Service]
User=whatsapi
WorkingDirectory=$DIR
ExecStart=$DIR/whatsapi
Restart=always
RestartSec=5
Environment=TZ=America/Sao_Paulo

[Install]
WantedBy=multi-user.target
UNIT

echo "==> Instalando o Caddy (HTTPS automático)..."
if ! command -v caddy >/dev/null; then
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq
  apt-get install -y -qq caddy >/dev/null
fi
cat > /etc/caddy/Caddyfile <<CADDY
$DOMAIN {
	reverse_proxy 127.0.0.1:8080
}
CADDY

echo "==> Liberando as portas 80 e 443 no firewall do servidor..."
for p in 80 443; do
  iptables -C INPUT -p tcp --dport $p -j ACCEPT 2>/dev/null || iptables -I INPUT 5 -p tcp --dport $p -j ACCEPT 2>/dev/null || iptables -I INPUT -p tcp --dport $p -j ACCEPT
done
command -v netfilter-persistent >/dev/null && netfilter-persistent save >/dev/null 2>&1 || true

systemctl daemon-reload
systemctl enable --now whatsapi >/dev/null 2>&1
systemctl restart whatsapi
systemctl enable caddy >/dev/null 2>&1
systemctl restart caddy

sleep 3
echo
echo "=============================================================="
if systemctl is-active --quiet whatsapi; then
  echo " WhatsAPI instalado e rodando!"
else
  echo " ATENÇÃO: o WhatsAPI não iniciou. Veja: sudo journalctl -u whatsapi -n 50"
fi
echo
echo " Painel / endereço da API:  https://$DOMAIN"
echo " Token da API:              $TOKEN"
echo
echo " (o HTTPS pode levar 1 minuto para ficar pronto na primeira vez)"
echo " Se o endereço não abrir, confira se as portas 80 e 443 estão"
echo " liberadas na Security List da Oracle (passo 4 do guia)."
echo "=============================================================="

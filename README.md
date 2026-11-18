# WhatsAPI

API REST própria de WhatsApp (estilo uazapi), feita em Go com [whatsmeow](https://github.com/tulir/whatsmeow).
Cada **instância** é um número conectado via QR Code (ou código de pareamento).

> ⚠️ É uma API **não oficial**. O WhatsApp pode banir números, principalmente com envio em massa
> ou para quem não te conhece. Use com moderação, de preferência num número que você pode perder.

## Rodando

### Windows (sem instalar nada)
1. Extraia `whatsapi-windows-amd64.zip` numa pasta, por exemplo `C:\whatsapi`.
2. Dê dois cliques em `whatsapi.exe`. Se o Windows mostrar "O Windows protegeu o computador",
   clique em **Mais informações > Executar assim mesmo** (o executável não é assinado).
   Se o firewall perguntar, permita o acesso.
3. Na primeira vez ele cria um token e salva no arquivo `.env` da mesma pasta. O token também aparece na janela preta.
4. Abra **http://localhost:8080** no navegador, cole o token, crie uma instância e escaneie o QR.

Deixe a janela aberta enquanto quiser a API funcionando (fechar = desligar).

### Linux / macOS
```bash
chmod +x whatsapi
./whatsapi
```
(no macOS, se bloquear: `xattr -d com.apple.quarantine whatsapi`)

### Configuração
Opcionalmente copie `.env.example` para `.env` na mesma pasta do executável e ajuste (porta, Postgres, segredo do webhook...).

As sessões ficam na pasta `data` ao lado do executável: reiniciar **não** desconecta os números.
Para mudar de computador, leve a pasta `data` junto. Se apagar a pasta, terá que escanear o QR de novo.

### Docker (opcional)
```bash
cp .env.example .env
docker compose up -d --build
```

### Compilando você mesmo
Precisa de Go 1.26+ (não precisa de gcc): `go build -o whatsapi .`

## Painel web
Em `http://localhost:8080` há um painel simples para criar instâncias, ver o QR, trocar webhook
e mandar uma mensagem de teste. Tudo que ele faz também dá para fazer pela API abaixo.

## Autenticação
Mande o token em qualquer um destes:
- cabeçalho `token: SEU_TOKEN`
- cabeçalho `Authorization: Bearer SEU_TOKEN`
- query `?token=SEU_TOKEN` (útil para abrir o QR no navegador)

## Conectando um número pela API

> No Windows, use o painel ou o `curl.exe` pelo PowerShell/Git Bash. Os exemplos abaixo são no formato bash.

```bash
TOKEN=seu-token   # está no .env
API=http://localhost:8080

# 1. Cria a instância (o webhook é opcional)
curl -X POST $API/instances -H "token: $TOKEN" \
  -d '{"name":"principal","webhook":"https://seu-servidor.com/webhook"}'

# 2. Inicia a conexão (gera o QR)
curl -X POST $API/instances/principal/connect -H "token: $TOKEN"
```

3. Abra no navegador (ou use o painel) e escaneie com o celular (WhatsApp > Aparelhos conectados > Conectar aparelho):
```
http://localhost:8080/instances/principal/qr?format=png&token=SEU_TOKEN
```
O QR muda a cada ~20s; atualize a página se expirar. Sem `format=png` a resposta vem em JSON com `qr` e `base64`.

**Alternativa sem QR** (código de 8 dígitos):
```bash
curl -X POST $API/instances/principal/paircode -H "token: $TOKEN" -d '{"phone":"5511999999999"}'
```

4. Confira: `curl $API/instances/principal -H "token: $TOKEN"` → `"status":"connected"`.

## Endpoints

| Método | Rota | O que faz |
|---|---|---|
| GET | `/health` | Healthcheck (sem token) |
| GET | `/instances` | Lista instâncias e status |
| POST | `/instances` | Cria `{name, webhook?}` |
| GET | `/instances/{name}` | Status (`disconnected`, `connecting`, `qr`, `connected`) |
| DELETE | `/instances/{name}` | Desloga e apaga |
| POST | `/instances/{name}/connect` | Conecta / gera QR |
| GET | `/instances/{name}/qr` | QR atual (`?format=png` para imagem) |
| POST | `/instances/{name}/paircode` | Código de pareamento `{phone}` |
| POST | `/instances/{name}/logout` | Desconecta o número (a instância continua) |
| PUT | `/instances/{name}/webhook` | Troca o webhook `{url}` (vazio desliga) |
| POST | `/instances/{name}/send/text` | `{to, text, quotedId?, mentions?}` |
| POST | `/instances/{name}/send/media` | `{to, type?, url \| base64, caption?, fileName?, mimetype?, ptt?}` |
| POST | `/instances/{name}/send/location` | `{to, latitude, longitude, name?, address?}` |
| POST | `/instances/{name}/send/reaction` | `{to, messageId, emoji, fromMe?, sender?}` |
| POST | `/instances/{name}/campaign` | Inicia disparo automático (veja abaixo) |
| GET | `/instances/{name}/campaign` | Progresso do disparo |
| DELETE | `/instances/{name}/campaign` | Para o disparo |
| POST | `/instances/{name}/check` | Verifica números `{numbers: [...]}` |
| GET | `/instances/{name}/groups` | Lista grupos |

**Campo `to`:** aceita número com DDI (`5511999999999`, pode ter `+`, espaços, traços),
JID (`5511999999999@s.whatsapp.net`) ou grupo (`123456789-123@g.us`).
Para números, a API consulta o WhatsApp antes e usa o JID real, então o problema do
9º dígito de alguns números do Brasil é resolvido sozinho.

### Exemplos
```bash
# Texto
curl -X POST $API/instances/principal/send/text -H "token: $TOKEN" \
  -d '{"to":"5511999999999","text":"Olá! 👋"}'

# Imagem por URL
curl -X POST $API/instances/principal/send/media -H "token: $TOKEN" \
  -d '{"to":"5511999999999","type":"image","url":"https://picsum.photos/600","caption":"Foto"}'

# PDF em base64
curl -X POST $API/instances/principal/send/media -H "token: $TOKEN" \
  -d '{"to":"5511999999999","type":"document","base64":"JVBERi0x...","fileName":"boleto.pdf","mimetype":"application/pdf"}'

# Áudio como mensagem de voz (precisa ser OGG/Opus)
curl -X POST $API/instances/principal/send/media -H "token: $TOKEN" \
  -d '{"to":"5511999999999","type":"audio","url":"https://.../audio.ogg","ptt":true}'
```

Resposta de envio: `{"id":"3EB0...","to":"5511999999999@s.whatsapp.net","timestamp":1763470000}`

## Compatível com Evolution API / WaDuck

Sistemas que já usavam Evolution API ou WaDuck funcionam só trocando a URL e a chave.
Aceita o cabeçalho `apikey` (o mesmo token) e as rotas com ou sem `/v1`:

| Rota | |
|---|---|
| `POST /v1/message/sendText/{instancia}` | `{number, text}` |
| `POST /v1/message/sendMedia/{instancia}` | `{number, mediatype, media, caption, fileName, mimetype}` |
| `POST /v1/instance/create` | `{instanceName, qrcode}` |
| `GET /v1/instance/connect/{instancia}` | QR em `base64` (ou `?number=` para código de pareamento) |
| `GET /v1/instance/connectionState/{instancia}` | `state`: `open`, `connecting` ou `close` |
| `GET /v1/instance/fetchInstances` | lista |
| `DELETE /v1/instance/logout/{instancia}` e `/v1/instance/delete/{instancia}` | |
| `POST /v1/webhook/set/{instancia}` | `{webhook: {url, enabled}}` |

Se o número estiver desconectado, o envio responde 500 com `"Connection Closed"`, igual à Evolution.

## Disparo automático

Pelo painel (seção "Disparo automático") ou pela API:

```bash
curl -X POST $API/instances/principal/campaign -H "token: $TOKEN" -d '{
  "numbers": ["5511999999999;João", "5521988887777;Maria"],
  "text": "Oi {nome}, tudo bem?",
  "delaySeconds": 60,
  "jitterSeconds": 15
}'
```

- Uma mensagem por vez, com `delaySeconds` de intervalo (padrão 60, mínimo 5) e uma variação aleatória de ±`jitterSeconds` (padrão 25% do intervalo), para não parecer robô.
- `{nome}` e `{numero}` são trocados pelos dados de cada linha. Números repetidos são ignorados.
- Se o número desconectar, o disparo pausa e continua sozinho quando reconectar.
- Uma campanha por instância. O progresso fica na memória: se fechar o programa, o disparo para.
- No fim, o webhook recebe o evento `campaign` com o resumo.

## Webhook

Cada instância pode ter uma URL. A API faz `POST` com JSON (até 3 tentativas se o seu servidor responder 5xx ou cair):

```json
{
  "instance": "principal",
  "event": "message",
  "timestamp": 1763470000,
  "data": {
    "id": "3EB0C767D26A1D5B",
    "chat": "5511999999999@s.whatsapp.net",
    "sender": "5511999999999@s.whatsapp.net",
    "from": "5511999999999",
    "pushName": "Fulano",
    "fromMe": false,
    "isGroup": false,
    "type": "text",
    "text": "Oi!"
  }
}
```

Eventos:
- `message`: `type` pode ser `text`, `image`, `video`, `audio`, `document`, `sticker`, `location`, `contact`, `reaction`, `button_reply`, `list_reply` ou `other`. Mídias trazem `mimetype` e `caption`, e `base64` se `WEBHOOK_MEDIA_BASE64=true`.
- `receipt`: `{chat, sender, ids, type: delivered|read|played}`
- `connection`: `{status: paired|connected|disconnected|logged_out}`

Se `WEBHOOK_SECRET` estiver definido, cada chamada leva `X-Signature-256: sha256=<hmac do corpo>` para você validar que veio da sua API.

Em grupos ou contatos novos o `sender` pode vir como `...@lid` (identificador oculto do WhatsApp); quando existir, o número real aparece em `senderAlt`.

## Variáveis de ambiente
Veja `.env.example`.

## Dicas para não tomar ban
- Não mande mensagem em massa para quem não te salvou.
- Espace os envios (alguns segundos entre mensagens).
- "Aqueça" números novos usando normalmente por alguns dias antes de automatizar.
- Para uso comercial sério, considere a WhatsApp Cloud API oficial da Meta.

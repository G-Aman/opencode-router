<div align="center">

  <img src="assets/logo.svg" alt="OpenCode Router Logo" width="128" height="128" />

  # OpenCode Router

  **High-performance, ultra-lightweight AI model gateway and proxy engine written in Go.**  
  *Unlocks official upstream models with sub-millisecond overhead, full OpenAI & Anthropic API compatibility, and an embedded WebUI.*

  <p>
    <a href="https://github.com/G-Aman/opencode-router/releases"><img src="https://img.shields.io/github/v/release/G-Aman/opencode-router?style=flat-square&color=0284c7" alt="Latest Release"></a>
    <a href="https://github.com/G-Aman/opencode-router/actions/workflows/release.yml"><img src="https://img.shields.io/github/actions/workflow/status/G-Aman/opencode-router/release.yml?style=flat-square" alt="Build Status"></a>
    <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.22-00ADD8?style=flat-square&logo=go" alt="Go Version"></a>
    <a href="https://github.com/G-Aman/opencode-router/pkgs/container/opencode-router"><img src="https://img.shields.io/badge/Docker-Multi--Arch-2496ED?style=flat-square&logo=docker" alt="Docker"></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-emerald?style=flat-square&color=10b981" alt="License"></a>
  </p>

</div>

---

## ⚡ Highlights

- **Near-Zero Footprint:** Compiled static Go binary consuming **< 13 MB RAM** and **0.0% idle CPU**. Runs smoothly on single-board computers and OpenWrt routers with as little as 64 MB RAM.
- **Full OpenAI & Anthropic Protocol Suite:** Native support for standard OpenAI `/v1/chat/completions`, OpenAI `/v1/responses` (Responses API for n8n AI agents & next-gen SDKs), and Anthropic `/v1/messages` (Claude Code, Cursor, Cline, LibreChat, OpenAI SDK, Anthropic SDK).
- **Tool Calling & Vision Support:** Full streaming & non-streaming support for Anthropic `tool_use` / `tool_result`, OpenAI function calling, and base64 multimodal vision inputs.
- **Outbound HTTP & SOCKS5 Proxy:** Built-in network routing through HTTP, HTTPS, SOCKS5, and SOCKS5h proxies with optional authentication.
- **Client Runtime Mimic:** Emulates official OpenCode CLI headers, preambles, and tool definitions to ensure 100% upstream model availability (including gated models like `mimo-*` and `nemotron-*`).
- **Responses Protocol Bridging:** Transparently bridges upstream SSE `/responses` models (`muse-*`) to standard client formats.
- **Smart Health Probing & Sync:** Continuous background health checks and model synchronization with zero runtime disk I/O on live traffic.
- **Embedded Control Dashboard:** Clean, responsive WebUI for real-time latency inspection, model catalog testing, one-click API URL copying, and dynamic configuration.

---

## 📸 WebUI Preview

<div align="center">
  <img src="assets/screenshots/preview-slideshow.gif" alt="OpenCode Router WebUI Auto Sliding Preview" width="880" style="border-radius: 8px; box-shadow: 0 4px 20px rgba(0,0,0,0.3);" />
</div>

<br/>

<details>
  <summary><b>🔍 Click to view high-resolution static slides manually</b></summary>
  <br/>

  #### 1. Overview & Real-Time Metrics
  <p align="center">
    <img src="assets/screenshots/dashboard.png" alt="Overview Dashboard" width="880" />
  </p>

  #### 2. Model Catalog & Status
  <p align="center">
    <img src="assets/screenshots/models.png" alt="Models Catalog" width="880" />
  </p>
</details>

---

## 🚀 Quick Start

### 1. Download Pre-Compiled Binaries

Download ready-to-run release packages directly from the [**Releases Page**](https://github.com/G-Aman/opencode-router/releases):

| OS / Target | Architecture | Direct Package Link |
| :--- | :--- | :--- |
| **Linux (Standard 64-bit)** | `x86_64 / amd64` | [Download AMD64 ZIP](https://github.com/G-Aman/opencode-router/releases/latest/download/opencode-router-linux-amd64.zip) |
| **Linux (ARM 64-bit)** | `arm64 / aarch64` | [Download ARM64 ZIP](https://github.com/G-Aman/opencode-router/releases/latest/download/opencode-router-linux-arm64.zip) |
| **Linux (ARM 32-bit)** | `armv7 / armv5` | [Download ARMv7 ZIP](https://github.com/G-Aman/opencode-router/releases/latest/download/opencode-router-linux-armv7.zip) · [ARMv5](https://github.com/G-Aman/opencode-router/releases/latest/download/opencode-router-linux-armv5.zip) |
| **OpenWrt Router** | `mipsle (MT7628 / TL-MR3020)` | [Download MIPSLE Soft-Float ZIP](https://github.com/G-Aman/opencode-router/releases/latest/download/opencode-router-linux-mipsle-softfloat.zip) |
| **OpenWrt Router** | `mipsle (MT7621)` | [Download MIPSLE Hard-Float ZIP](https://github.com/G-Aman/opencode-router/releases/latest/download/opencode-router-linux-mipsle-hardfloat.zip) |
| **OpenWrt Router** | `mips (Atheros AR9331)` | [Download MIPS Soft-Float ZIP](https://github.com/G-Aman/opencode-router/releases/latest/download/opencode-router-linux-mips-softfloat.zip) |
| **macOS (Apple Silicon)** | `M1 / M2 / M3 / M4 (arm64)` | [Download macOS ARM64 ZIP](https://github.com/G-Aman/opencode-router/releases/latest/download/opencode-router-darwin-arm64.zip) |
| **macOS (Intel)** | `x86_64 / amd64` | [Download macOS Intel ZIP](https://github.com/G-Aman/opencode-router/releases/latest/download/opencode-router-darwin-amd64.zip) |
| **Windows** | `64-bit (amd64)` | [Download Windows AMD64 ZIP](https://github.com/G-Aman/opencode-router/releases/latest/download/opencode-router-windows-amd64.zip) |
| **Windows (ARM)** | `arm64` | [Download Windows ARM64 ZIP](https://github.com/G-Aman/opencode-router/releases/latest/download/opencode-router-windows-arm64.zip) |

```bash
# Extract and launch (default port: 8787)
./opencode-router

# Or specify a custom listening port
./opencode-router 8790
```

### 2. Docker & Container Platforms

Run the official multi-arch container image (`linux/amd64`, `linux/arm64`, `linux/arm/v7`):

```bash
docker run -d \
  --name opencode-router \
  --restart unless-stopped \
  -p 8787:8787 \
  -v $(pwd)/opencode-router.json:/app/opencode-router.json \
  ghcr.io/g-aman/opencode-router:latest
```

Using Docker Compose:

```yaml
version: '3.8'
services:
  opencode-router:
    image: ghcr.io/g-aman/opencode-router:latest
    container_name: opencode-router
    restart: unless-stopped
    ports:
      - "8787:8787"
    volumes:
      - ./data:/app/data
```

### 3. Build from Source

Requirements: Go 1.22+

```bash
git clone https://github.com/G-Aman/opencode-router.git
cd opencode-router
go build -trimpath -ldflags="-s -w" -o opencode-router .
./opencode-router
```

---

## 🛠️ Usage with AI Clients

Set the Unified Base URL in your AI tool or client application:

* **Unified API Base URL:** `http://localhost:8787/v1` (or your server's LAN / Tailscale IP)
* **API Key:** Leave empty, or enter your configured `Proxy Key`

### 1. Anthropic SDK / Claude Code
```python
import anthropic

client = anthropic.Anthropic(
    base_url="http://localhost:8787",
    api_key="your-proxy-key" # or any non-empty string if proxy key is not set
)

response = client.messages.create(
    model="muse-spark-1.3-contributor-free",
    max_tokens=100,
    messages=[{"role": "user", "content": "Hello via Anthropic protocol!"}]
)
print(response.content[0].text)
```

### 2. OpenAI SDK
```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8787/v1",
    api_key="your-proxy-key"
)

response = client.chat.completions.create(
    model="nemotron-3.5-lightning-free",
    messages=[{"role": "user", "content": "Hello via OpenAI protocol!"}],
    stream=True
)

for chunk in response:
    print(chunk.choices[0].delta.content or "", end="", flush=True)
```

### 3. cURL (Anthropic Endpoint)
```bash
curl http://localhost:8787/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: your-proxy-key" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "space-bunny-free",
    "max_tokens": 100,
    "messages": [{"role": "user", "content": "Hi!"}]
  }'
```

---

## ⚙️ Configuration

The router auto-generates `opencode-router.json` on first run if not present. All settings can be adjusted on the fly from the WebUI:

```json
{
  "host": "0.0.0.0",
  "port": 8787,
  "proxyKey": "",
  "upstream": "https://opencode.ai/zen/v1",
  "defaultModel": "nemotron-3.5-lightning-free",
  "fallbackModels": [
    "ling-3.0-flash-fin-free",
    "nemotron-3-ultra-free",
    "muse-spark-1.3-contributor-free",
    "mimo-v2.6-flash-free"
  ],
  "modelAliases": {},
  "autoSyncModels": true,
  "autoSyncIntervalMs": 900000,
  "autoUA": true,
  "outboundProxy": "socks5://127.0.0.1:1080"
}
```

### Key Configuration Options:
* `host` *(string)*: Listening interface (`0.0.0.0` for all interfaces).
* `port` *(int)*: Listening port (default `8787`).
* `proxyKey` *(string)*: Optional authentication key for your gateway.
* `outboundProxy` *(string)*: HTTP or SOCKS5 proxy URL (`http://`, `https://`, `socks5://`, `socks5h://`) with optional user/password.
* `autoUA` *(bool)*: Automatically tracks official upstream releases and updates User-Agent headers.
* `fallbackModels` *(array)*: Priority order for automatic failover when a model encounters a 429 rate limit or outage.

---

## 📝 Changelog

### [v1.2.0] - Latest
- **Native OpenAI Responses API (`/v1/responses` & `/responses`):**
  - Full drop-in support for OpenAI Responses API payloads (string or typed `input` items, `instructions`, tools, streaming SSE, and non-streaming).
  - Strict compliance with OpenAI Responses SSE streaming specification (includes monotonic `sequence_number`, `item_id`, and `annotations` for seamless n8n AI Agent nodes and Vercel AI SDK compatibility).
  - Native dual routing: directly streams upstream Responses models (`muse-*`) and seamlessly translates between Responses format and Chat Completions format for standard chat models (`mimo-*`, `space-bunny`, `nemotron-*`, etc.).
- **Unified Authentication & BYOK Key Forwarding:**
  - Standardized client auth across all endpoints (`/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `/v1/models`).
  - Seamless pass-through of custom OpenCode/Zen keys (`zen_*`, `oc_*`) while allowing arbitrary dummy keys when `proxyKey` is unconfigured.
- **Robust Auto-Config Generation & In-Memory Defaults:**
  - Safeguarded in-memory defaults on initial run preventing empty protocol schemes on partial config updates.

### [v1.1.0]
- **Native Anthropic `/v1/messages` & `/messages` Compatibility:**
  - Full support for multi-turn conversations, SSE streaming, function/tool calling (`tool_use` & `tool_result`), and base64 multimodal vision.
  - Seamless drop-in compatibility for Claude Code, Cline, Cursor, LibreChat, and official Anthropic SDKs.
- **Outbound HTTP & SOCKS5 Proxy Support:**
  - Route upstream requests through HTTP, HTTPS, SOCKS5, or SOCKS5h proxies with authentication.
  - Configurable directly from the WebUI Settings dashboard (`outboundProxy`).
- **OpenCode CLI Mimicry & Gating Bypass:**
  - Automatically injects authentic developer system preambles and baseline tools to emulate the official OpenCode CLI client, eliminating upstream `FreeTierError` blocks on gated models (`mimo-*`, `nemotron-*`).
- **Responses Protocol Bridge:**
  - Transparently bridges upstream `/responses` SSE streams (e.g. `muse-spark`) for Anthropic clients.
- **Redesigned WebUI Settings & Stats Reset:**
  - Settings page structured into 4 responsive cards (*Network & Connectivity*, *Security & Rate Limiting*, *Model Routing & Rules*, *Health Probes & Auto-Sync*).
  - Added "Clear stats" button to reset request counters and recent metrics (`POST /api/reset`).
- **Security & Resilience Hardening:**
  - Constant-time secret comparison (`crypto/subtle.ConstantTimeCompare`) on all authentication tokens.
  - 32 MB request body limit via `http.MaxBytesReader` to prevent memory exhaustion DoS.
  - Zero disk I/O during active streaming and chat traffic (pure in-memory mutex tracking with selective persistence on manual tests and sync cycles).

### [v1.0.0]
- Initial release of OpenCode Router.
- Full OpenAI `/v1/chat/completions` and `/v1/models` compatibility.
- Real-time WebUI dashboard with live traffic feed, latency tracking, and model catalog.
- Automatic upstream model discovery, health probing, and dynamic failover routing.
- Multi-arch Docker images and pre-compiled static binaries for Linux, OpenWrt (MIPS/MIPSLE), macOS, and Windows.

---

## 📄 License

This project is licensed under the [MIT License](LICENSE).

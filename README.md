# tether

`tether` es un servidor CLI/TUI multiplataforma escrito en **Go** que actúa como *side-car* y puente de ejecución de terminales (PTY) para sistemas agénticos multi-agente (como `localloop`, IDEs y sub-agentes).

Corre como una única instancia central en un puerto HTTP fijo (por defecto `127.0.0.1:7432`) y permite que múltiples agentes y sub-agentes se conecten simultáneamente mediante **MCP sobre HTTP / SSE (Streamable HTTP y SSE)**, manteniendo sus sesiones de terminal estrictamente aisladas por identidad de agente (`agent_id`) y acotadas a su `workspace_root` autorizado, todo supervisado en tiempo real por un operador humano a través de una interfaz **TUI multi-pestaña** construida con `bubbletea`.

---

## Estructura del Proyecto

```text
tether/
├── cmd/tether/
│   └── main.go               # Punto de entrada, flags CLI (--port, --ttl, --idle-timeout) y orquestación
├── internal/
│   ├── bus/                  # Event Bus interno (Pub/Sub con canales Go) y cola FIFO de consentimientos
│   ├── env/                  # Detección de OS, arquitectura, shells instaladas y validación de rutas
│   ├── auth/                 # Handshake POST /auth/connect, tokens HMAC-SHA256 con TTL y middleware HTTP
│   ├── guard/                # Command Guard con parser AST (mvdan.cc/sh/v3/syntax) y seguimiento de CWD
│   ├── pty/                  # Pool PTY aislado por AgentID (go-pty) + Job Objects/PGID + RingBuffer + ANSI strip
│   ├── mcp/                  # Servidor MCP HTTP/SSE (mark3labs/mcp-go) y handlers de Tools/Resources
│   └── tui/                  # Aplicación Bubbletea (Header, pestañas PTY por agente y cola inferior FIFO)
├── go.mod
└── README.md
```

---

## Uso y Flags CLI

```bash
# Compilar binario
go build -o tether ./cmd/tether

# Ejecutar con configuración por defecto (127.0.0.1:7432, TTL 30m)
./tether --port 7432 --ttl 30m --idle-timeout 3s
```

---

## Flujo de Conexión y Endpoints HTTP/MCP

1. **Handshake con Consentimiento Explícito (`POST /auth/connect`):**
   ```json
   {
     "agent_id": "localloop-coder-01",
     "agent_name": "LocalLoop Coder",
     "workspace": "/ruta/al/workspace"
   }
   ```
   La petición queda bloqueada hasta que el humano la aprueba en la barra inferior de la TUI (`[A]probar` o `[S]esión`), devolviendo un `access_token` firmado con `HMAC-SHA256` y caducidad explícita (`TTL`).
2. **Conexión MCP Autenticada (`Authorization: Bearer <access_token>`):**
   * **Streamable HTTP:** `POST /mcp`
   * **SSE + Message:** `GET /sse` y `POST /message`

### Primitivas MCP
* **Resources:**
  * `tether://env/system`: OS, arquitectura, shells detectados y `workspace_root` asignado al agente.
  * `tether://sessions`: Lista de sesiones PTY activas del agente autenticado, `rel_cwd` y estado (`IDLE`, `RUNNING`, `WAITING_FOR_INPUT`).
* **Tools:**
  * `session_open(shell?, name?)`
  * `session_exec(session_id, command, timeout_ms?)`
  * `session_input(session_id, text, press_enter?)`
  * `session_screen(session_id, lines?)`
  * `session_close(session_id)`

---

## Atajos de Teclado en la TUI

* **`Tab` / `Shift+Tab`**: Cambiar entre pestañas PTY de los agentes conectados.
* **`A`**: Aprobar una vez (`ALLOW_ONCE`) la solicitud en la cabeza de la cola FIFO.
* **`S`**: Permitir para toda la sesión (`ALLOW_SESSION`).
* **`D`**: Denegar (`DENY`) la solicitud actual.
* **`Ctrl+T`**: Activar/desactivar modo de intervención directa humana en la PTY seleccionada (`Enter` para enviar input, `Ctrl+C` para enviar `SIGINT`, `Esc` para salir del modo input).
* **`Ctrl+X`**: Cerrar la pestaña PTY activa y matar su árbol de subprocesos.
* **`Q` / `Ctrl+C`**: Cerrar `tether` de forma ordenada terminando todas las sesiones PTY activas.

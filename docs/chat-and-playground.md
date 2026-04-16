# Chat & Playground

LLM API Proxy includes a built-in chat interface with persistent sessions. Chat history is stored in SQLite and survives server restarts.

## Chat Sessions

The chat system provides persistent, multi-turn conversations through the web UI. Each session stores its own messages, model selection, and parameters.

### Features

- **Persistent sessions** — Chat history survives server restarts (stored in SQLite)
- **Session management** — Create, rename, and delete sessions
- **Multi-turn conversations** — Full conversation history maintained per session
- **Auto-generated titles** — Session titles are generated using an LLM (configurable)
- **Default model** — Set a default model for all new sessions

### Configuration

```yaml
server:
  # Path to the chat database (default: data/chat.db)
  chat_db_path: "data/chat.db"

  # Model used to auto-generate session titles
  # Must be a valid model ID available through your backends
  title_model: "glm-5-turbo"
```

### Session Title Generation

When a new session is created, the proxy can automatically generate a descriptive title by sending the first message to the configured `title_model`. If `title_model` is not set, titles default to a truncated version of the first message.

You can also change the title model from the web UI without restarting.

### Web UI

Navigate to **Chat** (`/ui/chat`) in the web dashboard to use the chat interface.

Available actions:

- **New Session** — Start a new conversation
- **Model Selection** — Choose from all available models
- **Session List** — View and switch between sessions
- **Rename** — Edit session titles
- **Delete** — Remove individual or all sessions
- **Parameters** — Set temperature, top_p, max_tokens per session

## Playground

The Playground (`/ui/playground`) provides a quick way to test models interactively:

1. Select a model from the dropdown
2. Type a message and get a streaming response
3. View token usage and latency for each request

Unlike Chat sessions, Playground interactions are not persisted — they're designed for quick testing and experimentation.

## API Endpoints

All chat endpoints are under `/ui/chat`:

| Endpoint                          | Method | Description                     |
| --------------------------------- | ------ | ------------------------------- |
| `/ui/chat`                        | GET    | Chat page                       |
| `/ui/chat/models`                 | GET    | Available models for chat       |
| `/ui/chat/sessions`               | GET    | List all sessions               |
| `/ui/chat/sessions`               | POST   | Create new session              |
| `/ui/chat/sessions/{id}`          | GET    | Get session details             |
| `/ui/chat/sessions/{id}`          | PUT    | Update session (rename, params) |
| `/ui/chat/sessions/{id}`          | DELETE | Delete session                  |
| `/ui/chat/sessions`               | DELETE | Delete all sessions             |
| `/ui/chat/sessions/{id}/messages` | GET    | List messages in session        |
| `/ui/chat/sessions/{id}/messages` | POST   | Save message to session         |
| `/ui/chat/sessions/{id}/title`    | POST   | Generate session title via LLM  |
| `/ui/chat/title-model`            | PUT    | Set title generation model      |
| `/ui/chat/default-model`          | PUT    | Set default model for new chats |

## Data Storage

Chat data is stored in SQLite with two tables:

- **Sessions**: id, title, model, system_prompt, temperature, top_p, max_tokens, created_at, updated_at
- **Messages**: id, session_id, role, content, tokens, prompt_tokens, model, duration_ms, created_at

Messages are cascade-deleted when their parent session is deleted.

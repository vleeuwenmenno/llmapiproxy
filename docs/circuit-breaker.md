# Circuit Breaker

The circuit breaker automatically suspends backends that return consecutive rate-limit (429) responses, preventing wasted requests and improving overall latency.

## How It Works

Each backend has an independent circuit breaker with three states:

| State                   | Behavior                                                                                                                                            |
| ----------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Closed** (healthy)    | Requests flow normally. Consecutive 429 counter increments on rate limits.                                                                          |
| **Open** (suspended)    | Backend is skipped entirely. No requests are sent. Waits for cooldown to expire.                                                                    |
| **Half-Open** (probing) | Cooldown expired. One request is allowed through to test if the backend has recovered. If it succeeds → back to Closed. If it fails → back to Open. |

## Configuration

```yaml
routing:
  circuit_breaker:
    enabled: true # Enable/disable circuit breaker (default: true)
    threshold: 3 # Consecutive 429s before tripping (default: 3)
    cooldown: 300 # Seconds to keep backend suspended (default: 300 = 5 min)
```

| Option      | Type | Default | Description                                                     |
| ----------- | ---- | ------- | --------------------------------------------------------------- |
| `enabled`   | bool | `true`  | Enable or disable the circuit breaker                           |
| `threshold` | int  | `3`     | Number of consecutive 429 responses before tripping the breaker |
| `cooldown`  | int  | `300`   | Seconds to keep a tripped backend in Open state before probing  |

### Retry-After Header

When a backend returns a 429 with a `Retry-After` header, the circuit breaker uses that value as the cooldown **if** it's less than 2 hours. Otherwise, the configured `cooldown` is used.

## Web UI

The circuit breaker card on the dashboard shows:

- Current state for each backend (Closed/Open/Half-Open)
- Consecutive failure count
- Time until the next probe attempt

### Management Endpoints

| Endpoint                   | Method | Description                                 |
| -------------------------- | ------ | ------------------------------------------- |
| `/ui/circuit/card`         | GET    | Circuit breaker card (HTMX fragment)        |
| `/ui/circuit/states`       | GET    | All circuit breaker states (JSON)           |
| `/ui/circuit/reset/{name}` | POST   | Manually reset a specific breaker to Closed |
| `/ui/circuit/reset-all`    | POST   | Reset all breakers to Closed                |
| `/ui/circuit/config`       | POST   | Update thresholds dynamically               |

### Dynamic Configuration

You can update circuit breaker settings from the web UI without editing the config file:

1. Navigate to the dashboard
2. Find the Circuit Breaker card
3. Adjust threshold and cooldown values
4. Click Save

## Interaction with Routing

When a circuit breaker is **Open**, the backend is effectively removed from the routing pool:

- **Priority:** The backend is skipped; the next available backend is tried
- **Round-Robin:** The backend is skipped; rotation continues among healthy backends
- **Race / Staggered-Race:** The backend is excluded from the parallel launch

When the breaker transitions to **Half-Open**, the backend is re-added to the routing pool for a single test request. If the test succeeds, the breaker resets to Closed and the backend fully rejoins.

## Example Scenario

```
1. Backend "openrouter" returns 429 (count: 1)
2. Backend "openrouter" returns 429 (count: 2)
3. Backend "openrouter" returns 429 (count: 3) → Circuit trips to Open
4. All subsequent requests skip "openrouter" for 5 minutes
5. After 5 minutes → Half-Open
6. Next request to "openrouter" succeeds → Closed (count: 0)
```

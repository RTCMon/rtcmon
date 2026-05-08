# RTCMon (Backend)

RTCMon is a high-performance, self-hosted WebRTC monitoring and analytics platform. This repository contains the core API services, ingestion engine, and data models required to collect, process, and store real-time WebRTC metrics from client applications.

> [!NOTE]
> This repository contains the **API/Backend only**. For the JavaScript SDK and Dashboard UI, please refer to their respective repositories.

---

## 🚀 Overview

WebRTC applications often suffer from a "monitoring gap" where traditional health checks show green while users experience dropped calls or choppy media. RTCMon bridges this gap by collecting per-session, per-connection metrics directly from the browser's `getStats()` API, providing deep observability into network quality, media performance, and connection lifecycles.

## ✨ Key Features

- **High-Performance Ingest**: Go-based API capable of handling tens of thousands of concurrent sessions with minimal latency.
- **Observations Layer**: Rule-based detection that automatically flags packet loss spikes, jitter, RTT issues, and media freezes.
- **eMOS Scoring**: Automated computation of Estimated Mean Opinion Score (eMOS) to quantify user experience.
- **Hierarchical Data Model**: Organizes data into Organizations → Apps → Conferences → Participants → Sessions → Connections.
- **Scalable Storage**: Optimized for time-series data using PostgreSQL and efficient batch processing.
- **Caching & Rate Limiting**: Integrated Redis support for state management and API protection.

---

## 🏗 Architecture

RTCMon follows a decoupled architecture designed for scale and resilience.

```mermaid
graph TD
    subgraph "Client Side"
        SDK["JS SDK"]
    end

    subgraph "RTCMon Backend"
        Ingest["Ingest API (Go/Chi)"]
        Worker["Worker Pool (Goroutines)"]
        Query["Query API (Go/Chi)"]
    end

    subgraph "Storage & Cache"
        Postgres[("PostgreSQL")]
        Redis[("Redis")]
    end

    subgraph "Frontend"
        Dash["Dashboard (Vue 3)"]
    end

    SDK -- "POST /v1/events (JWT Auth)" --> Ingest
    Ingest -- "Internal Channel" --> Worker
    Worker -- "Batch Write" --> Postgres
    Ingest -- "Rate Limit / Cached Claims" --> Redis
    Dash -- "GraphQL / REST" --> Query
    Query -- "Read" --> Postgres
```

---

## 📊 Metrics Collected

RTCMon captures comprehensive statistics across four primary dimensions:

| Dimension | Key Metrics |
|-----------|-------------|
| **Network Quality** | RTT, Jitter, Packet Loss Rate, Bitrate (In/Out), Available Bandwidth |
| **Media Quality** | FPS (Send/Recv), Resolution, Video Freezes, Audio Energy, Concealment Ratio |
| **Lifecycle** | ICE Gathering/Connection States, DTLS Transitons, Signaling States |
| **Context** | Browser, OS, Geolocation (City/Country), Network Type (WiFi/Cellular) |

---

## 🛠 Prerequisites

- **Go**: 1.25+
- **Docker**: 20.10+
- **Docker Compose**: 3.9+
- **Makefile**: For standard development tasks

---

## 🚀 Quick Start

### 1. Clone & Start Services

The fastest way to get started is using Docker Compose. This starts PostgreSQL, Redis, and the Ingest API.

```bash
docker compose up -d
```

### 2. Run Database Migrations

Ensure your database schema is up-to-date using the provided Makefile command:

```bash
make migrate-up
```

### 3. Verify Health

```bash
curl http://localhost:8080/health
```

---

## ⚙️ Configuration

RTCMon is configured using environment variables. You can set these in a `.env` file or directly in your shell.

| Variable | Description | Default |
|----------|-------------|---------|
| `DB_URL` | PostgreSQL connection string | `postgres://postgres:postgres@localhost:5432/rtcmon` |
| `REDIS_URL` | Redis connection string | `redis://localhost:6379/0` |
| `AUTH_JWT_SECRET` | Secret key for JWT verification | **REQUIRED** |
| `SERVER_PORT` | Port for the Ingest API | `8080` |
| `LOG_LEVEL` | Logging level (`debug`, `info`, `warn`, `error`) | `info` |
| `WORKER_COUNT` | Number of ingestion workers | `NumCPU * 2` |
| `WORKER_BATCH_SIZE` | Max rows per DB batch insert | `500` |

---

## 🔨 Development

The `Makefile` contains common commands for development:

- `make build`: Build all Go modules.
- `make build-api`: Build the ingestion API binary.
- `make test`: Run the test suite with race detection.
- `make lint`: Run `golangci-lint` to verify code quality.
- `make tidy`: Tidy up Go modules.
- `make clean`: Remove build artifacts.

---

## 📄 License

Distributed under the MIT License. See `LICENSE` for more information (if applicable).

---

## 🤝 Contributing

1. Fork the Project
2. Create your Feature Branch (`git checkout -b feature/AmazingFeature`)
3. Commit your Changes (`git commit -m 'Add some AmazingFeature'`)
4. Push to the Branch (`git push origin feature/AmazingFeature`)
5. Open a Pull Request

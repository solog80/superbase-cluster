# StationPlaylist Traffic Ingestion & Affidavit System: Installation & Setup Guide

This guide covers everything required to deploy the automated StationPlaylist broadcast log pipeline, from the Windows studio playout PC to the TimescaleDB cluster on QNAP, and how the Traffic Officer accesses automated affidavits.

---

## 1. What Is Needed (Prerequisites)

### A. On the Windows Playout PC (`AIR-PC` at the FM Station)
* **Windows OS**: Windows 10, Windows 11, or Windows Server.
* **StationPlaylist Studio**: Studio v5.x / v6.x generating daily spot logs (`SLog-YYMMDD.csv` or `*.log`).
* **Network Connectivity**: Outbound HTTPS access to `edge.solofx.net` (or internal network access to QNAP via Tailscale/LAN).
* **Package File**: `StationPlaylist.TrafficAgent-win-x64.zip` (already built and packaged in `dist/`).
* *No Go runtime, .NET runtime, or Python installation is required on the Windows PC.*

### B. On the QNAP NAS (`ug` node / `nas-ts`)
* **TimescaleDB Instance**: Running on port `55439` with database `analytics` (Tailscale IP: `100.116.185.70`).
* **PostgreSQL / TimescaleDB Access**: `psql` client or admin credentials (`postgres`).

### C. On the Mesh Primary (`us1` / `Edge`)
* **Go Service**: `salt-gofn` listening on `:8080` (behind Envoy/Caddy at `https://edge.solofx.net/api/v1`).
* **Docker / Compose**: To redeploy the `supabase-api` container.

---

## 2. Step-by-Step Installation

```
Step 1: Database Migration ──► Step 2: Mesh Service Deploy ──► Step 3: Windows Agent Setup ──► Step 4: Traffic Officer Verification
  (QNAP TimescaleDB)               (Edge Server)                      (Playout PC)                       (Browser / Excel)
```

---

### Step 1: Run the Database Migration on QNAP TimescaleDB

The migration file creates the `radio_spot_logs` hypertable, unique deduplication indexes, advertiser resolution rules (`traffic_client_rules`), and the SQL functions for affidavit generation.

From your Mac terminal (or SSH into the QNAP host):

```bash
psql -h 100.116.185.70 -p 55439 -U postgres -d analytics \
  -f /Users/solomacbookair/Documents/myApps/superbase-cluster/migrations/tsdb_009_stationplaylist_traffic.sql
```

#### What this sets up:
* `public.radio_spot_logs`: TimescaleDB hypertable partitioned on `played_at`.
* `public.traffic_client_rules`: Pattern table seeded with Salt FM advertisers (MTN, Minute Maid, Airtel, Plascon, Syova, Ebenezer, etc.).
* `public.get_traffic_advertiser_summary()`: Generates client summaries and spot totals.
* `public.get_traffic_affidavit()`: Generates certified airtime records mapped to EPG shows.

---

### Step 2: Deploy the Mesh Service (`salt-gofn`) to Edge

The Go backend endpoints (`/api/v1/ingestTrafficLogs`, `/api/v1/getTrafficAdvertisers`, `/api/v1/getTrafficAffidavit`, `/api/v1/uploadTrafficLogFile`) are already written and compiled to `salt-gofn-linux`.

From your Mac terminal:

```bash
# 1. Copy the compiled Linux binary to the Edge primary
scp /Users/solomacbookair/Documents/myApps/superbase-cluster/gofn/salt-gofn-linux Edge:/opt/supabase-gofn/salt-gofn

# 2. SSH into Edge and rebuild/restart the container
ssh Edge
cd /opt/supabase-gofn
docker build -t salt-gofn:new .
docker compose up -d --no-deps supabase-api
```

#### Verify the endpoint is alive:
```bash
curl -I https://edge.solofx.net/api/v1/getTrafficAdvertisers
# Expected: HTTP/2 200 (or application/json [] if no logs ingested yet)
```

---

### Step 3: Install the Agent on the Windows Playout PC

You have two deployment options on the Windows machine. **Option A is recommended** for resilience and performance.

#### Option A: Standalone Agent (`StationPlaylistTrafficAgent.exe`)

1. Copy [`StationPlaylist.TrafficAgent-win-x64.zip`](file:///Users/solomacbookair/Documents/myApps/stationplaylist-agent-windows/StationPlaylist.TrafficAgent-win-x64.zip) to the Windows PC (e.g. via network share or flash drive).
2. Extract the archive to `C:\StationPlaylist\TrafficAgent\`.
3. Open `config.json` in Notepad and verify the paths:
   ```json
   {
     "log_dir": "C:\\StationPlaylist\\Logs",
     "endpoint": "https://edge.solofx.net/api/v1/ingestTrafficLogs",
     "station_id": 307,
     "poll_interval_seconds": 15,
     "filter_adverts": true,
     "state_file": ".spl_agent_state.json"
   }
   ```
   *(Note: If logs are stored on another drive or share, adjust `log_dir`, e.g. `"F:\\StationPlaylist\\Logs"` or `"\\\\AIR-PC\\Air Data\\Logs"`).*

4. **Test the agent with Dry-Run Mode** (prints parsed adverts without sending):
   ```cmd
   cd C:\StationPlaylist\TrafficAgent
   StationPlaylistTrafficAgent.exe --dry-run
   ```
   You should see output matching:
   ```text
   [Dry-Run] SLog-261014.csv: Found 120 new advert plays:
     #1 [14/10/2026 06:16:28] MTN RB Lug (Dur: 56s, DJ: Default User)
     #5 [14/10/2026 06:31:22] EBENEZER HARDWARE (Dur: 61s, DJ: Default User)
   ```

5. **Historical Backfill** (Upload past days/weeks of logs):
   ```cmd
   StationPlaylistTrafficAgent.exe --backfill --no-watch
   ```

6. **Set Up Auto-Start on Windows Boot**:
   * **Simple Setup (Windows Startup Folder)**:
     1. Press `Win + R`, type `shell:startup`, and press Enter.
     2. Right-click → **New** → **Shortcut**.
     3. Browse to `C:\StationPlaylist\TrafficAgent\StationPlaylistTrafficAgent.exe`.
   * **Enterprise Setup (Background Windows Service via NSSM)**:
     ```cmd
     nssm.exe install StationPlaylistTrafficAgent "C:\StationPlaylist\TrafficAgent\StationPlaylistTrafficAgent.exe"
     nssm.exe set StationPlaylistTrafficAgent AppDirectory "C:\StationPlaylist\TrafficAgent"
     nssm.exe start StationPlaylistTrafficAgent
     ```

---

#### Option B: Zero-Install PowerShell Script (No `.exe` Required)

If you prefer not to run an executable file on the playout machine:

1. Copy [`scripts/StationPlaylist-TrafficSync.ps1`](file:///Users/solomacbookair/Documents/myApps/stationplaylist-agent-windows/scripts/StationPlaylist-TrafficSync.ps1) to `C:\StationPlaylist\Scripts\StationPlaylist-TrafficSync.ps1`.
2. Open **Windows Task Scheduler**:
   - Create a task named `StationPlaylist-TrafficSync`.
   - **Trigger**: Daily, repeat every **15 minutes** indefinitely.
   - **Action**: Start a program:
     - Program: `powershell.exe`
     - Arguments: `-ExecutionPolicy Bypass -WindowStyle Hidden -File "C:\StationPlaylist\Scripts\StationPlaylist-TrafficSync.ps1"`

---

## 3. How the Traffic Officer Uses It

The Traffic Officer no longer needs access to the on-air playout PC or manual spreadsheets.

### 1. Check Active Advertisers & Monthly Totals
Run in terminal or open in browser / Postman:
```bash
curl "https://edge.solofx.net/api/v1/getTrafficAdvertisers?days=30"
```
**Returns clean JSON summary:**
```json
[
  {
    "client": "MTN",
    "total_spots": 25,
    "total_airtime_seconds": 1344,
    "total_airtime_minutes": 22.4,
    "creative_count": 4,
    "creatives": ["MTN Boda Code Lug", "MTN RB Lug", "Salt and MTN"]
  },
  {
    "client": "Ebenezer Hardware",
    "total_spots": 15,
    "total_airtime_seconds": 915,
    "total_airtime_minutes": 15.3,
    "creative_count": 1,
    "creatives": ["EBENEZER HARDWARE"]
  }
]
```

### 2. Download Certified Client Affidavit as CSV (For Billing Invoices)
To generate proof of performance for **MTN** for October 2026:
```bash
curl "https://edge.solofx.net/api/v1/getTrafficAffidavit?client=MTN&start=2026-10-01&end=2026-10-15&format=csv" \
  -o "MTN_Affidavit_October2026.csv"
```
This downloads a formatted CSV ready to open in **Microsoft Excel** or attach to client invoice emails:

| Air Date & Time (EAT) | Client | Ad Title | Duration | Show / Program | DJ / Playout | Audio File |
|---|---|---|---|---|---|---|
| 2026-10-14 06:16:28 | MTN | MTN RB Lug | 00:56 | Morning Drive | Default User | F:\Adverts\MTN RB Lug.mp3 |
| 2026-10-14 06:30:26 | MTN | MTN RB Lug | 00:56 | Morning Drive | Default User | F:\Adverts\MTN RB Lug.mp3 |
| 2026-10-14 08:16:58 | MTN | MTN Boda Code Lug | 00:55 | Zibulatudde | Default User | F:\Adverts\MTN Boda Code Lug.mp3 |

### 3. One-Click Manual Log File Upload
If the traffic officer has an isolated log file (`SLog-261014.csv`):
```bash
curl -F "file=@SLog-261014.csv" "https://edge.solofx.net/api/v1/uploadTrafficLogFile"
```
Response:
```json
{"success": true, "parsed": 120, "inserted": 120}
```

---

## 4. How to Add or Update Advertiser Matching Rules

When a new advertiser signs a contract and spots are added to `F:\Adverts\`, you can map them in the database without touching any code:

Connect to TimescaleDB and insert a rule:
```sql
INSERT INTO public.traffic_client_rules (client_name, match_pattern, category, priority)
VALUES ('Stanbic Bank', 'STANBIC', 'Commercial', 100);
```
From that moment onward, any playout file or title containing `STANBIC` will automatically attribute to `Stanbic Bank`.

---

## 5. Troubleshooting & FAQ

| Problem | Cause | Solution |
|---|---|---|
| **Agent logs "Failed to push: connection refused"** | Studio internet down or mesh unreachable | The agent leaves the watermark at the last successful line and retries every 15s. No spots are lost. |
| **A spot shows under "Other Advert"** | Filename or title didn't match existing client rules | Add a pattern for that brand to `public.traffic_client_rules`. |
| **New daily log at midnight not syncing** | StationPlaylist rolled over to `SLog-YYMMDD.csv` | The agent automatically detects the date change, finishes yesterday's file, and switches to today's file. |
| **Duplicate spot prevention** | The agent ran twice or backfilled an already synced day | `public.radio_spot_logs` enforces a unique index on `(played_at, log_hash)`. Duplicate entries are automatically skipped. |

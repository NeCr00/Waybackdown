# waybackdown

Download historical web snapshots from multiple public archives using a **host-first strategy** that minimises CDX requests.

Instead of one archive query per URL, the tool extracts all unique hostnames from the input list, queries each archive **once per host** to retrieve its full URL inventory, then matches that inventory against the user's list — dramatically reducing API calls when many input URLs share the same domain.

## Install

**From source** (requires Go 1.21+):
```bash
go install github.com/NeCr00/Waybackdown@latest
```

## Usage

```
waybackdown -u <url> [-u <url> ...] [options]
waybackdown -l <file> [options]
cat urls.txt | waybackdown [options]
```

| Flag | Default | Description |
|------|---------|-------------|
| `-u` | | URL to download (repeatable: `-u url1 -u url2`) |
| `-l` | | File with one URL per line |
| `-mode` | `newest` | `oldest` · `newest` · `all` |
| `-o` | `waybackdown_output` | Output directory |
| `-c` | `10` | Hosts processed concurrently per provider |
| `-max` | `0` (unlimited) | Max snapshots per URL in `all` mode |
| `-status` | `` (all) | Filter by HTTP status at capture time (e.g. `200`) |
| `-providers` | `wayback,archiveph,commoncrawl,arquivo` | Provider priority order |
| `-rps` | `5.0` | Requests/second for downloads + non-CC CDX (0 = unlimited) |
| `-burst` | `20` | Rate limiter burst size |
| `-cc-rps` | `5.0` | Common Crawl CDX requests/second (independent of `-rps`) |
| `-cc-burst` | `20` | CC CDX rate limiter burst size |
| `-cc-max` | `3` | Max Common Crawl index collections to query per host |
| `-host-limit` | `100000` | Max CDX records per host inventory query (0 = no limit) |
| `-dl-workers` | `4` | Parallel download workers per URL in `all` mode |
| `-timeout` | `30s` | Per-request HTTP timeout |
| `-retries` | `3` | Retries on transient failures |
| `-v` | | Verbose output |

## Examples

```bash
# Newest snapshot of a single URL
waybackdown -u https://target.com

# All historical versions of a URL, verbose
waybackdown -u https://target.com -mode all -v

# Only successful (200 OK) captures
waybackdown -u https://target.com/login.php -mode all -status 200

# Multiple URLs via repeated -u flags (single host → 1 CDX query)
waybackdown -u https://target.com -u https://target.com/login -u https://target.com/admin -mode newest

# Bulk list: one host-level query covers all URLs from the same domain
waybackdown -l urls.txt -mode newest -o ./archives

# Piped input from another tool
cat urls.txt | waybackdown -mode all -status 200

# Wayback + Common Crawl only
waybackdown -l urls.txt -providers wayback,commoncrawl -rps 10 -burst 40
```

## How it works

```
Input URLs  →  extract unique hosts  →  deduplicate
                        │
             ┌──────────▼──────────┐
             │  Provider 1 (Wayback)│
             │  query: host/*       │  ← one CDX request per host
             │  match user URLs     │
             │  download matches    │
             └──────────┬──────────┘
                        │ unresolved URLs only
             ┌──────────▼──────────┐
             │  Provider 2 (archiveph) │
             │  per-URL fallback    │  ← no host query support
             └──────────┬──────────┘
                        │ unresolved URLs only
             ┌──────────▼──────────┐
             │  Provider 3 (CC)    │  ← host/* across all collections
             └──────────┬──────────┘
                        │ still not found
                   "not found in any archive"
```

**Request savings example:** 500 URLs from `example.com` → 1 CDX request instead of 500.

## Output structure

```
waybackdown_output/
└── target.com/
    └── path/to/page/
        ├── 20230101120000_200.html
        └── 20210615093012_200.html
```

Files are written atomically. Re-running skips already-downloaded snapshots (resume-safe).

## Providers

| Provider | Source | Host-level query |
|----------|--------|-----------------|
| `wayback` | web.archive.org CDX API | ✓ (`url=host/*`) |
| `archiveph` | archive.ph Memento timemap | ✗ (per-URL fallback) |
| `commoncrawl` | index.commoncrawl.org CDX + WARC byte-range | ✓ (`url=host/*` per collection) |
| `arquivo` | arquivo.pt CDX API | ✓ (`url=host/*`) |

Providers are tried in priority order. Each provider only receives URLs not resolved by earlier providers. For providers without host-level support, individual URL queries are used as fallback.

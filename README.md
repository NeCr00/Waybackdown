# waybackdown

Download historical web snapshots from multiple public archives.
Queries Wayback Machine, archive.ph, Common Crawl, and Arquivo.pt in fallback order.

## Install

**From source** (requires Go 1.21+):
```bash
go install github.com/NeCr00/Waybackdown@latest
```

## Usage

```
waybackdown -u <url> [options]
waybackdown -l <file> [options]
```

| Flag | Default | Description |
|------|---------|-------------|
| `-u` | | Single URL to download |
| `-l` | | File with one URL per line |
| `-mode` | `newest` | `oldest` · `newest` · `all` |
| `-o` | `waybackdown_output` | Output directory |
| `-c` | `5` | Concurrent URL workers |
| `-max` | `0` (unlimited) | Max snapshots per URL in `all` mode |
| `-status` | `` (all) | Filter by HTTP status at capture time (e.g. `200`) |
| `-providers` | `wayback,archiveph,commoncrawl,arquivo` | Fallback order |
| `-rps` | `2.0` | Max requests/second (0 = unlimited) |
| `-burst` | `10` | Rate limiter burst size |
| `-timeout` | `30s` | Per-request HTTP timeout |
| `-retries` | `3` | Retries on transient failures |
| `-cc-max` | `3` | Max Common Crawl collections to query per URL |
| `-v` | | Verbose output |

## Examples

```bash
# Newest snapshot of a single URL
waybackdown -u https://target.com

# All historical versions, verbose
waybackdown -u https://target.com -mode all -v

# Only successful (200 OK) captures
waybackdown -u https://target.com/login.php -mode all -status 200

# Bulk download from a URL list, 10 workers
waybackdown -l urls.txt -mode all -c 10 -o ./archives

# Wayback + Common Crawl only, higher rate limit
waybackdown -l urls.txt -providers wayback,commoncrawl -rps 5 -burst 20
```

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

| Provider | Source |
|----------|--------|
| `wayback` | web.archive.org CDX API |
| `archiveph` | archive.ph Memento timemap |
| `commoncrawl` | index.commoncrawl.org CDX + WARC byte-range |
| `arquivo` | arquivo.pt CDX API |

Providers are tried in order; the first one returning results wins.

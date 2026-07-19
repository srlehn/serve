# serve

Share the current directory over HTTP/HTTPS: browse, upload, download, fetch a
folder as a `.tar.zst`, or move files between devices with QR codes. On startup
it prints the reachable URLs, each with a scannable QR code.

## Usage

```
cd the/directory
go run github.com/srlehn/serve@latest
```

Listens on `:8000` (HTTP) and `:8443` (HTTPS, self-signed).

Flags:

- `-upload-limit` maximum upload size, e.g. `500MB`, `2GiB` (`0` = unlimited)
- `-browser-log` log browser scanner diagnostics

## License

[MIT](LICENSE), Copyright (c) 2022, 2026 Simon Robin Lehn

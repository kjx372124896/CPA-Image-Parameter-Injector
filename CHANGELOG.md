# Changelog

## 0.2.0

- Support JSON image generation requests at `/v1/images/generations`.
- Support multipart image editing requests at `/v1/images/edits`.
- Inject size, n, aspect ratio, quality, style, output format and background constraints into the prompt when present and enabled.
- Preserve image and mask multipart file parts, filenames, MIME headers and binary contents.
- Keep original request fields and make prompt injection idempotent.
- Add tests for multipart preservation and the request interceptor RPC envelope.

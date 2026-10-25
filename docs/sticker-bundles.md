# Sticker catalogue bundles

A generated sticker bundle is a directory with a `manifest.json` file and the
static WebP files named by that manifest. Bundle files are served from disk by
range, so the server does not copy the catalogue's artwork into memory.

The manifest is UTF-8 JSON with version `1` and a `packs` array:

```json
{
  "version": 1,
  "packs": [
    {
      "short_name": "openmoji_animals",
      "source": "OpenMoji Animals",
      "license": "CC-BY-SA-4.0",
      "attribution": "Emoji by OpenMoji",
      "stickers": [
        {
          "emoji": "🐈",
          "file": "openmoji_animals/cat.webp",
          "size": 1234,
          "sha256": "<64 lowercase hexadecimal characters>"
        }
      ]
    }
  ]
}
```

Each pack needs a unique lowercase short name, source, attribution and an SPDX
license from this allow-list: `CC0-1.0`, `CC-BY-4.0`, `CC-BY-SA-4.0`,
`Apache-2.0`, `MIT`, or `OFL-1.1`. The server displays the title with a
readable license name, such as `<source> (CC BY-SA 4.0)`; it must fit
Telegram's 64-character limit. Packs may
contain 1 to 120 stickers. Each sticker's file path is relative to the bundle
directory, and its declared size and SHA-256 must match a regular WebP file
inside that directory.

Validate a bundle before publishing it:

```bash
go run ./cmd/stickerbundle-check /path/to/sticker-bundle
```

To enable the bundle, set `TG_STICKER_CATALOG_BUNDLE` to its directory. The
server validates the complete bundle before starting. With the setting absent
or empty, it serves only the built-in `twemoji_reactions` set.

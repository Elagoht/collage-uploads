# elagoht/uploads

A collage plugin that receives file uploads, checks each file by its content,
stores it under a random key, and serves it back without handing an attacker
your origin. It reads a form the application already parsed, or streams the body
part by part, and both give the same result.

```sh
go get github.com/Elagoht/collage-uploads
```

Requires collage v0.58.0 or later, whose `WithStreamingBody()` lets an action
receive its body unread.

```go
u := uploads.New(uploads.Dir("var/uploads"))

app, err := collage.New(&collage.Config{
	Plugins: []collage.Plugin{u},
})

upload := func(_ context.Context, rc *collage.RenderContext) (*collage.ActionResult, error) {
	got, err := u.Receive(rc,
		uploads.Field("avatar", uploads.Rules{
			MaxSize:  5 << 20,
			MaxFiles: 1,
			Types:    []string{"image/png", "image/jpeg"},
		}),
	)
	var rule *uploads.Error
	if errors.As(err, &rule) {
		// Show rule.Field's error beside the form; got.Values holds what was typed.
		return showFormAgain(rc, got.Values, rule)
	}
	if err != nil {
		return nil, err
	}
	avatar := got.Files["avatar"][0] // Key, Name, Type, SHA256, Size
	if err := saveAvatar(rc.Context(), userID(rc), avatar); err != nil {
		_ = u.Store().Delete(rc.Context(), avatar.Key)
		return nil, err
	}
	return collage.SeeOther("/profile"), nil
}
```

## Two paths

`Receive` reads the request the way it arrives.

- **A parsed form.** When the action's form has already been parsed
  (`rc.Request.MultipartForm` is set, as it is for an ordinary action whose
  token came in the form field), `Receive` reads the files from it. A plain
  HTML `<form enctype="multipart/form-data">` with `{{csrfToken}}` works this
  way, with no JavaScript. The core parser holds up to 32 MiB in memory and
  spills the rest to temp files it removes afterwards, so this suits files up to
  tens of MB, within the action's `MaxBodyBytes`.
- **A stream.** On an action built `WithStreamingBody()`, nothing reads the
  body before the handler, and `Receive` walks it with `MultipartReader`,
  streaming each file straight into the store. Nothing is held whole in memory
  or spilled to disk outside the store. Use it for large files.

```go
collage.NewAction("upload").WithPath("en", "/upload").WithMethods(http.MethodPost).
	WithStreamingBody().
	WithMaxBodyBytes(2 << 30).        // the 4 MiB default bounds a stream too
	WithBodyTimeout(5 * time.Minute). // the server's 15s/30s deadlines end a long upload
	WithHandler(upload)
```

See [Timeouts](#timeouts) for choosing the timeout.

A streaming action takes its CSRF token from the `X-CSRF-Token` header only, so a
plain form cannot post to it. Send it with `fetch()`, reading the token from the
input `{{csrfToken}}` renders (the cookie is `HttpOnly`). `X-CSRF-Token` and
`_csrf` are the defaults; `Security.CSRFHeaderName` and `Security.CSRFFieldName`
rename them.

```js
const token = form.querySelector('input[name="_csrf"]').value;
await fetch(form.action, {
  method: "POST",
  headers: {"X-CSRF-Token": token},
  body: new FormData(form),
});
```

Anything that parses the form on a streaming action before `Receive`, such as
`FormValue`, `ParseMultipartForm` or `validate.Form`, gives up the stream without
an error. `Receive` still succeeds, through the parsed path, but by then net/http
has written the whole body, up to `MaxBodyBytes`, to temp files under
`os.TempDir()` — on many Linux hosts a `tmpfs`, and so memory. The other order
fails instead: after `Receive` has streamed the body, `validate.Form` finds an
empty form. Read and check the non-file fields from `Received.Values`.

## Rules

Each file field is declared with `uploads.Field(name, uploads.Rules{...})`. A
file in a field that was not declared is refused.

| Rule | Default | |
| --- | --- | --- |
| `MaxSize` | `0`: no per-file limit | The most bytes one file may have. Exactly `MaxSize` is accepted; reading stops one byte past it. The action's `MaxBodyBytes` applies either way |
| `MaxFiles` | `1` | How many files the field may carry. A field sent twice counts each file |
| `Types` | empty: any type | Media types allowed, without parameters (`"image/png"`, `"text/plain"`) |

The type is sniffed from the file's first 512 bytes with
`http.DetectContentType`. The client's file name, extension and `Content-Type`
are never consulted: HTML or SVG sent as `image/png` with a `.png` name sniffs as
text, and an image-only rule refuses it. An empty file is
`application/octet-stream`. There is no SVG signature: SVG sniffs as `text/xml`
or `text/plain`, so it passes only a rule that allows those.

## Errors

A broken rule is an `*uploads.Error` with the `Field` that broke it and an `Err`
of:

| Error | |
| --- | --- |
| `ErrTooLarge` | A file over its field's `MaxSize`, or non-file values over 64 KiB in total |
| `ErrType` | A file whose sniffed type is not in its field's `Types` |
| `ErrTooMany` | A field carrying more than its `MaxFiles` files |
| `ErrUnexpectedField` | A file in a field `Receive` was not given |

Use `errors.Is(err, uploads.ErrType)` to tell them apart, and `errors.As` for the
field. With an `*Error`, `Receive` also returns a `Received` holding no files and
the values read so far, for showing the form again. On the stream path those are
only the values that came before the failing part. When several parts break
rules, the stream path reports the first in the body; the parsed path checks
values, then undeclared fields, then files field by field in name order.

Other errors come back with a nil `Received`:

- a second `Receive` in one render, or one after the application called
  `rc.Request.MultipartReader()` itself, is `ErrAlreadyRead`. After the
  application parsed the form (`ParseMultipartForm`, `FormValue`), `Receive`
  succeeds through the parsed path, as above; after it read the body some other
  way (`io.ReadAll`), the stream path fails with the multipart reader's error;
- a body over the action's `MaxBodyBytes` wraps `*http.MaxBytesError`, which
  collage answers with 413;
- a client that went away, or a store that failed, is returned as it came.

## All or nothing

If anything fails, every file that call stored is deleted before `Receive`
returns: a broken rule, a client that disconnected, a body cut off by its limit,
a store that misreports what it wrote, or a panic out of the store. A stream that
ends mid-file leaves no file in the store and no temp file on disk.

Once `Receive` succeeds, the files are yours. If the action fails after that, as
in a database write, delete them with `u.Store().Delete`.

A crash, or a shutdown that times out, between `Receive` and your database write
leaves files in the store that no row points to. They are harmless, since nobody
holds their keys, but they take space. Sweep for them now and then: list the
store's directory (the `Store` interface has no listing; `Dir` is a plain
directory), and delete each key older than an hour or so that your database does
not know. Skip `.upload-*` temp files younger than that, which may be uploads in
progress.

## Keys and names

- **`Key`** is where the store keeps the file: 32 hex characters from
  `crypto/rand` (128 bits), then an extension for the sniffed type (`.png`,
  `.jpg`, `.gif`, `.webp`, `.pdf`, `.txt`, anything else `.bin`). Nothing the
  client sent goes into it.
- **`Name`** is the client's file name, cleaned. It keeps the part after the last
  `/` or `\`; drops invalid UTF-8, control and format characters (a right-to-left
  override included) and line separators; maps `.` and `..` to `""`; and is cut
  to 255 bytes on a character boundary. It is for display. Never use it as a
  path.
- **`Type`** is the sniffed type, parameters included
  (`text/plain; charset=utf-8`); **`SHA256`** is the hex digest of the stored
  bytes, computed as they stream; **`Size`** is the byte count.

## Metadata is yours to keep

The store keeps bytes under keys and nothing else. The name, type, hash, size,
owner and upload time are in the `File` that `Receive` returns. Put them in your
database next to the key. The plugin has no listing, no lookup by name and no
record of who uploaded what.

## Dir

`uploads.Dir(path)` is the disk store:

- **Permissions.** The directory is created 0700 on the first write, and an
  existing one with any group or other bit set is tightened to 0700 (the write
  fails if it cannot be). Files are 0600.
- **Never overwrites.** A file is written to a temp file in the same directory,
  synced, then hard-linked to its key, so a reader never sees a half-written file
  and two writes of one key cannot both win. On a filesystem without hard links
  (FAT, some network or FUSE mounts) it falls back to an exclusive create and a
  copy. That still never overwrites, but a concurrent read can see the file while
  it is being written. An existing key is `ErrExists`.
- **Keys.** Only `[a-z0-9_.-]`, 1 to 255 bytes, with no leading or trailing dot,
  no `..` and no Windows device name (`con`, `nul.png`, ...). Anything else is
  `ErrBadKey`. Uppercase is refused so two keys never name one file on a
  case-insensitive disk.
- **Confined.** Reads and deletes go through an `os.Root` of the directory and
  act only on regular files. A symlink or a directory under a key is not found
  by `Open` and is refused by `Delete`.
- **Crashes.** A crash mid-write can leave a `.upload-*` temp file. No key can
  name it, and it is safe to delete.

Built with `New(nil)` or `NewWith`, the plugin makes a `Dir` from its
configuration:

```json
{"elagoht/uploads": {"dir": "var/uploads"}}
```

`dir` defaults to `"var/uploads"`, or to what `NewWith` was given. Setting it to
`""` is a startup error. A store passed to `New` ignores the configuration. Any
type implementing the `Store` interface (`Put`, `Open`, `Delete`) can stand in
for `Dir`.

## Serving

`u.Handler()` serves stored files by key, for `GET` and `HEAD`. Mount it under a
prefix ending in `/`, from the application:

```go
app.Handle("/uploads/", u.Handler())
```

or from a plugin's `Init`, with `host.Handle("/uploads/", u.Handler())`.

The key is the last segment of the decoded path, so no `http.StripPrefix` is
needed. A key `Dir` would refuse is 404, and so is one that is not stored, a
directory, or a symlink. Encoded traversal (`..%2f`, `%5c`) and uppercase keys
are 404. Any other store error is 500 and goes to the application's logger; the
client sees no detail. Other methods are 405 with `Allow: GET, HEAD`.

Every file is answered with:

| Header | Value |
| --- | --- |
| `Content-Type` | The type sniffed again from the stored bytes, never the key's extension |
| `X-Content-Type-Options` | `nosniff` |
| `Content-Security-Policy` | `sandbox` |
| `Content-Disposition` | `attachment; filename="<key>"`, except for `image/png`, `image/jpeg`, `image/gif` and `image/webp`, which are inline. SVG and HTML are always attachments |
| `Cache-Control` | `private, max-age=0`, or `HandlerOptions.CacheControl` |

```go
u.Handler(uploads.HandlerOptions{CacheControl: "private, max-age=86400"})
```

An empty `CacheControl` keeps the default. The file name in `Content-Disposition`
is the key, since the handler does not know the original name. To download under
the original name, serve the file from an action of your own that looks the name
up in your database.

`Dir`'s files can seek, so a single `Range` and conditional requests
(`If-Modified-Since`) work, through `http.ServeContent`, with the same headers. A
request for several ranges is served whole, because a `multipart/byteranges`
answer would replace the sniffed `Content-Type`. A store whose reader cannot seek
is served whole, and `Range` is ignored.

- **`Last-Modified` is the upload time.** `Dir`'s files carry their write time,
  and ServeContent sends it, so anyone with a key learns when the file was
  uploaded.
- **A 416 carries no `Cache-Control`.** A range that cannot be satisfied is
  answered by `net/http`, which drops `Cache-Control` from its error response;
  `nosniff`, `sandbox` and `Content-Disposition` stay, and the body is a short
  error, not the file.
- **A failed seek is not shown.** If the store's reader fails to seek while a
  range is served, the real error is logged with the key, and the client sees
  only a generic one.

The handler does no access control. Anyone with a key can fetch the file: 128
random bits are hard to guess, but a key that leaks (in a log, in a `Referer`) is
a link anyone can use. For private files, serve them from an action that checks
who is asking and then calls `u.Store().Open`, setting the same headers.

## Security notes

- **A file is typed by its content, twice.** `Receive` sniffs to apply the rules;
  the handler sniffs again to answer. A key's extension or a renamed file changes
  nothing.
- **Polyglots.** A file that starts with a PNG signature and carries HTML after
  it sniffs as `image/png` and is served inline. With `nosniff` the browser
  treats it as an image only, and `sandbox` means that even a document opened
  from it runs no script and gets an opaque origin. Process images (re-encode
  them, as an image pipeline does) if you need the bytes themselves clean.
- **A separate origin is safer still.** The headers above keep a served file from
  acting as your site. Serving uploads from a domain of their own
  (`usercontent.example.com`), with no cookies of your main site, means a browser
  bug or a missed header leaks nothing.
- **Names are display text.** Escape `Name` like any other user input when you
  show it; templates do this for you.

## Timeouts

The server's deadlines are sized for pages: `ReadTimeout` (15s by default) bounds
reading the whole request, and `WriteTimeout` (30s) starts when the headers
arrive, so it bounds the upload as well as the answer. A streaming upload slower
than that fails. Raising only `ReadTimeout` is worse: an upload that outlives
`WriteTimeout` is received and stored, the client never gets the answer, and it
tries again.

Give the upload action its own deadline with `WithBodyTimeout(d)` (collage
v0.57.0) rather than raising the server's. Both deadlines become now plus `d` for
that action alone, after its page's guards and, on a streaming action, after the
`X-CSRF-Token` check, so a request without a token is refused under the server's
deadlines. Choose `d` to cover the whole upload and the answer: at least
`MaxBodyBytes` divided by the slowest connection you serve. When a client goes
away mid-upload, or runs out of time, `Receive` deletes what it stored, and collage
records a `400` or `408` and logs it at debug rather than as a server error. The
client itself usually receives nothing: it is gone, or its write deadline passed
with the read one.

## Behind a proxy

- **nginx** refuses anything over `client_max_body_size` (1 MiB by default) with
  its own 413, and by default buffers the whole body to its own disk before
  passing it on, which loses the stream and fills nginx's temp directory instead.
  For the upload location:

  ```nginx
  location /upload {
      client_max_body_size     2g;
      proxy_request_buffering  off;
      proxy_send_timeout       30m;
      proxy_read_timeout       30m;
      proxy_pass               http://127.0.0.1:8080;
  }
  ```

- **Caddy** streams request bodies; `request_body { max_size 2GB }` sets its
  limit.
- **Cloudflare** caps request bodies by plan (100 MB on Free and Pro when this was
  written; check Cloudflare's documentation for yours), and other CDNs and load
  balancers have caps and idle timeouts of their own. Larger files need a
  hostname that bypasses the CDN.

collage's "Going to production" guide has the same notes under "Uploads".

## Abuse and disk

An upload action takes bytes from the network and keeps them on your disk, so it
is the first thing an abuser looks for.

- **Guard the action.** The example above has no guard. A CSRF token is not
  authentication: anyone who loads the form page gets one. Put the upload action
  on a page behind a session or auth guard, or behind an auth middleware, so an
  anonymous client cannot store anything. An action registered on its own has no
  page guards, and a check inside the handler runs only after `WithBodyTimeout`
  has granted the long deadline.
- **Quotas are the application's.** The plugin has no notion of a user. Check
  each user's total in the handler before calling `Receive`, from your database;
  `rc.Request.ContentLength`, when the client sends it, is an upper bound to check
  first.
- **Keep the limits tight.** `MaxBodyBytes` and each field's `MaxSize` are what one
  request can cost you. Set them to the largest file you mean to accept, not to
  what the disk could take.
- **Rate-limit the path.** `elagoht/ratelimit` on the upload action bounds how
  often one client can try.
- **Watch the disk.** `Dir` has no total cap. Keep it on a filesystem or volume
  of its own, so a full store does not stop the rest of the system, and monitor
  it.
- **Long uploads hold slots.** `elagoht/health`'s `maxInFlight` counts an upload
  for as long as it lasts. Size it for the uploads you expect at once, or slow
  uploads will push the rest of the site into 503s.
- **Never serve the directory directly** with nginx or a static file handler:
  that drops the headers `u.Handler()` sets.

## Limits

- Non-file values: 64 KiB in total, counting each name and 200 bytes per part.
- The stream path reads at most 1000 parts.
- The sniff window is 512 bytes.
- `MaxSize` per file, and the action's `MaxBodyBytes` (4 MiB unless raised) for
  the whole body.
- One `Receive` per request.

## Out of scope

- S3 and other remote stores. The `Store` interface is ready for them, as
  plugins of their own.
- Resumable or chunked uploads (tus).
- Image processing (see opti-image).
- Virus scanning.
- Client-side progress UI.
- Quotas and per-user limits (see [Abuse and disk](#abuse-and-disk)).

## Changes

### v0.1.1

- collage.json is regenerated for editors. Nothing else changes.
- Requires collage v0.58.0.

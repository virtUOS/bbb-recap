# Install the BBB plugin

The plugin adds an **AI minutes** control ("KI-Protokoll" in German) to the BBB nav bar, in the place of BBB's recording
indicator, which it hides in recap meetings. The AI minutes are the normal BBB recording of the meeting: the plugin
starts and pauses it with the same call as BBB's record button. Users who may record (moderators, or users with the join parameter
`userdata-bbb_record_permission=true`, in meetings with `record=true` and `allowStartStopRecording=true`) get a pill
that starts and pauses; everyone else gets a status chip once the minutes have been started. The plugin is active
only in meetings created with `meta_recap-enabled=true`. It sends no data outside BBB; the minutes come from the
recording through the exporter ([install-exporter.md](install-exporter.md)) as before. What it shows, per role and
state, is in [What the plugin shows](#what-the-plugin-shows).

You need BBB 4.0 (plugin SDK `1.0.0-beta.3`) and a static HTTPS host. The plugin is built and unit-tested but not
yet tested on a BBB server.

`RecapPlugin.js` bundles the BBB plugin SDK (LGPL-3.0), React and browser-bunyan (MIT). The comment at the top of the
file and [`plugin/NOTICE`](../../plugin/NOTICE) list versions, licenses and sources.

## Quick start

1. Download the plugin from the [GitHub release](https://github.com/virtUOS/bbb-recap/releases) and check it, on
   any machine with curl. Releases carry the plugin from `v0.2.1` on; for `v0.1.0` and `v0.2.0` use
   [Build it yourself](#build-it-yourself) instead.

   ```sh
   VERSION=v0.2.1                              # same release as the service
   RELEASE_URL=https://github.com/virtUOS/bbb-recap/releases/download/$VERSION
   mkdir "recap-plugin-$VERSION" && cd "recap-plugin-$VERSION"
   curl -fsSL --fail-early -O "$RELEASE_URL/RecapPlugin.js" -O "$RELEASE_URL/manifest.json" \
     -O "$RELEASE_URL/recap-plugin-$VERSION.zip" -O "$RELEASE_URL/SHA256SUMS"
   sha256sum -c SHA256SUMS                     # expect three lines ending in OK
   ```

   The zip holds the same two files.

2. Upload both files into one directory on a static HTTPS host and check that they are served:

   ```sh
   PLUGIN_URL=https://cdn.example.org/bbb-recap/$VERSION      # the directory that holds both files
   curl -fsS "$PLUGIN_URL/manifest.json"                      # expect the manifest
   curl -fsSI "$PLUGIN_URL/RecapPlugin.js" | grep -i '^content-type'   # expect: text/javascript or application/javascript
   ```

   bbb-web fetches `manifest.json` when a meeting is created; the browsers of the participants load `RecapPlugin.js`
   from the same directory.

3. Register the manifest on each BBB server in bbb-web's `pluginManifests` setting, a JSON list. **Packaged BBB**, as
   root:

   ```sh
   PLUGIN_URL=https://cdn.example.org/bbb-recap/v0.2.1      # from step 2
   PROPS=/etc/bigbluebutton/bbb-web.properties
   if grep -q '^pluginManifests=' "$PROPS"; then
     grep '^pluginManifests=' "$PROPS"   # already set: add {"url":"$PLUGIN_URL/manifest.json"} to this list,
                                         # then run: systemctl restart bbb-web
   else
     echo "pluginManifests=[{\"url\":\"$PLUGIN_URL/manifest.json\"}]" >> "$PROPS" && systemctl restart bbb-web
   fi
   ```

   **Docker BBB** (bigbluebutton PR #26186 setup): bbb-web takes any key of its properties from the environment
   (PR #26186 @ `152e63c`, `docker/README.md`, "Environment variables"). Add the `web` service to the
   `docker-compose.override.yml` that [install-exporter.md](install-exporter.md) step 4 created. This writes the whole
   file: the exporter's three `recording-workers` mounts and the plugin. If your file has more entries, add the `web`
   block by hand instead.

   ```sh
   BBB_DIR=/srv/bbb                                          # holds BBB's docker-compose.yml
   PLUGIN_URL=https://cdn.example.org/bbb-recap/v0.2.1      # from step 2
   cat "$BBB_DIR/docker-compose.override.yml"               # check: only the exporter's recording-workers mounts
   cat > "$BBB_DIR/docker-compose.override.yml" <<EOF
   services:
     recording-workers:
       volumes:
         - /etc/recap-exporter/recap_export.rb:/usr/local/bigbluebutton/core/scripts/post_archive/recap_export.rb:ro,z
         - /etc/recap-exporter/exporter.env:/etc/recap-exporter/exporter.env:ro,z
         - /var/lib/recap-exporter/spool:/var/lib/recap-exporter/spool:z
     web:
       environment:
         PLUGINMANIFESTS: '[{"url":"$PLUGIN_URL/manifest.json"}]'
   EOF
   cd "$BBB_DIR" && docker compose up -d web recording-workers
   ```

4. Check: create a meeting with `meta_recap-enabled=true`, `record=true` and `allowStartStopRecording=true` (a front
   end with recap does this) and join as moderator. The grey pill "Start AI minutes" appears at the start of the
   nav bar's right group, and BBB's own recording pill is not shown. A viewer sees nothing until the minutes are started, then the chip "Minutes are being taken". In a
   meeting without `meta_recap-enabled` the plugin is not loaded; with any value other than `true` it loads but shows
   nothing.

## Build it yourself

Instead of step 1, on any machine with Node.js 24 or later and git. The release workflow
(`.github/workflows/release.yaml`) runs the same commands; `npm version` gives the plugin the release's version.

```sh
VERSION=v0.2.0
git clone --depth 1 --branch "$VERSION" https://github.com/virtUOS/bbb-recap.git
cd bbb-recap/plugin
npm ci && npm version "${VERSION#v}" --no-git-tag-version --allow-same-version && npm run build
ls dist                                     # expect: RecapPlugin.js  manifest.json
```

Continue with step 2, uploading the two files from `dist/`.

## What the plugin shows

The language follows the user's BBB locale: German for `de` and its regions, English for every other locale. The
timer counts active time only (not the pauses) and is the same in every client: it is computed from the recording
data on the server clock, with the formula of BBB's own recording timer.

**Users who may record: the pill** (36 px high, like BBB's recording pill).

| State   | Pill                                                                      | Click                       |
| ------- | ------------------------------------------------------------------------- | --------------------------- |
| Off     | grey, outline sparkle, "Start AI minutes"                                 | opens the start popover     |
| Running | violet, filled sparkle with a pulsing ring, "AI minutes · mm:ss"          | opens the pause popover     |
| Paused  | grey with a dashed violet border, pause sign, "AI minutes paused · mm:ss" | resumes at once, no popover |

The start popover says what is recorded (everything said, with names and timestamps, no video in the minutes; chat,
participant list and meeting data) and that everyone sees a notice; the pause popover says that nothing is
transcribed until the minutes are resumed. Neither says where the minutes go.

**Everyone else: the chip** (32 px, violet border), hidden while the minutes are off. Running: "Minutes are being
taken" with a pulsing dot; paused: "Minutes paused" with a hollow dot. A click opens an info popover with what is
recorded.

**Toasts**, in every client, on a change only (not for the state found on joining):

| Change      | Users who may record                    | Everyone else                       |
| ----------- | --------------------------------------- | ----------------------------------- |
| first start | "AI minutes are running"                | "This meeting is now being minuted" |
| resume      | none (whoever resumed has just clicked) | "This meeting is now being minuted" |
| pause       | "AI minutes paused"                     | "AI minutes paused"                 |

**Motion and accessibility:** the pulse is an SVG animation and is left out when the system asks for reduced motion
(`prefers-reduced-motion: reduce`); BBB's own "animations" setting is not available to plugins. The pill and the chip
are buttons whose tooltip, which BBB also uses as the accessible name, states the state and what a click does (the
nav bar passes no `aria-pressed`). The popovers are modal dialogs: the focus moves into them, Tab stays inside, Escape
or a click outside closes them, and the focus returns to the pill or chip. BBB shows every toast with `role="alert"`;
a plugin cannot change that.

## CSS the plugin adds

While active (only in recap meetings), the plugin adds one `<style data-recap-plugin>` element to the page and
removes it when it stops:

```css
[data-test="recordingIndicator"] {
  display: none !important;
}
[id="success-record-app.notification.recordingStart"],
[id="error-record-app.notification.recordingPaused"] {
  position: absolute !important;
  visibility: hidden !important;
  pointer-events: none !important;
}
dialog[data-recap-popover]::backdrop {
  background: transparent;
}
```

- The first rule hides BBB's own recording pill in the nav bar (the "Start recording" button and the recording
  indicator with its timer), for moderators and viewers alike. In a recap meeting the recording is the AI minutes, and
  the plugin's pill or chip takes its place: the plugin's item comes right after it in the nav bar, so with BBB's pill
  hidden it is the first item of the right group. `data-test="recordingIndicator"` sits only on the wrapper of that
  pill. `display: none` takes it out of the layout, the tab order and the accessibility tree, and no animation needs
  to finish. BBB's start-recording confirmation opens only from a click or Enter on that pill, so it cannot appear.
  BBB's consent dialog (`notifyRecordingIsOn`) is a separate element and is not hidden; leave `notifyRecordingIsOn` off
  for recap meetings.
- The second rule hides BBB's own toasts "This session is now being recorded" and "This session is not being
  recorded anymore". In a recap meeting the recording is the AI minutes, and the plugin's own toasts already report
  both changes. The selectors are the toast ids BBB builds from the server notification (type, icon, message id), so
  each matches exactly one toast and nothing else. Every other toast stays as it is. The toasts are made invisible rather than removed
  with `display: none`, so their timer and close animation still run and they go away as usual; screen readers do
  not announce them.
- The third rule keeps the page undimmed behind the plugin's own popovers.

If a BBB upgrade changes the `data-test` or the toast ids, the rule matches nothing and BBB's pill or toasts show
again next to the plugin's; nothing else breaks. A Content Security Policy that forbids inline styles has the same effect, and the page is then
also dimmed behind the plugin's popovers. Sources (BBB v4.0.0
`7d27e7e`, unchanged in v4.0.2): `nav-bar/nav-bar-graphql/recording-indicator/component.tsx` (the pill),
`SetRecordingStatusCmdMsgHdlr.scala` (the notifications), `bigbluebutton-html5/
imports/ui/services/notification/index.js` (`generateToastId`), react-toastify 11.0.2 `Toast.tsx` (`id={toastId}`).

## How the plugin is limited to recap meetings

`manifest.json` contains the placeholder `${meta_recap-enabled}` without a default. bbb-web fills it from the
meeting's `meta_recap-enabled` parameter and does not load the plugin when the parameter is missing; the client only
writes a debug log. bbb-web logs an ERROR ("Missing required metadata … Plugin not loaded.", with a stack trace) for
every meeting created without the parameter; this is expected. To avoid it, register the plugin per meeting instead
(below). bbb-web passes the value on in the script URL, and the plugin stays idle unless it is exactly
`true` ([metadata-interface.md](../reference/metadata-interface.md) §1.2). So the global `pluginManifests` entry is
enough: no change in the front end. Sources: BBB `docs/docs/plugins.md` ("Meta\_ parameters", "Default value
(fallback) for missing placeholder's parameters") and `bbb-common-web/.../api/util/PluginUtils.java` at BBB v4.0.x.

## Other ways to host

- **On the BBB server, packaged BBB:** copy both files to `/var/www/bigbluebutton-default/assets/plugins/recap/`; the
  manifest URL is then `https://$BBB_HOST/plugins/recap/manifest.json` (BBB `docs/docs/plugins.md`, "Hosting the
  Plugin on a BBB Server").
- **On the BBB server, docker BBB** (PR #26186 setup): its nginx serves `/var/www/bigbluebutton-default/assets` as
  well (`docker/compose/nginx/bigbluebutton`). Copy both files to `$BBB_DIR/plugins/recap/` and mount that directory
  by adding `nginx` to `docker-compose.override.yml` (next to the services from step 3), then
  `docker compose up -d nginx`:

  ```yaml
  services:
    nginx:
      volumes:
        - ./plugins/recap:/var/www/bigbluebutton-default/assets/plugins/recap:ro,z
  ```

- **Per meeting instead of server-wide:** a front end can pass `pluginManifests=[{"url":"…/manifest.json"}]` on
  `create` of recap meetings only; BBB merges it with the server-wide list.

## Upgrade

Download (or build) the new release, upload it to a new directory (`$PLUGIN_URL` with the new version), change the
URL in `pluginManifests` and restart bbb-web as in step 3. Running meetings keep the old version. The manifest carries
the plugin version, which BBB appends to the script URL, so browsers do not use a cached old script. With
`pluginManifestCacheEnabled=true`, bbb-web may keep an old manifest for up to
`pluginManifestCacheRefreshIntervalMinutes`.

The plugin relies on BBB internals that are not part of the plugin SDK's contract: the recording GraphQL fields
(`meeting_recording`, `current_time`), the nav bar rendering a label made of elements and the inline style of a
nav bar button, and the `data-test` and toast ids of the hidden BBB elements above. Re-check after every BBB upgrade, in a recap meeting:

- the pill in all three states and the chip look as described above, and the timer is the same in two clients;
- BBB's recording pill is not visible and not reachable with Tab, for moderators and viewers, in every state;
- starting and pausing show the plugin's toasts and **not** BBB's "being recorded" toasts;
- the pill keeps the keyboard focus while the timer runs.

`requiredSdkVersion` in the manifest (`^1.0.0-beta.3`) makes BBB refuse to load the plugin with an incompatible SDK.

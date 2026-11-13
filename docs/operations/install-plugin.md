# Install the BBB plugin

The plugin adds an **AI summary** button to the BBB nav bar, right of BBB's recording indicator. It shows the state
("AI summary: off" or "on") and, after a confirmation, starts or pauses the normal BBB recording, exactly like BBB's
record button, which stays. It shows only to users who may record (moderators, or users with the join parameter
`userdata-bbb_record_permission=true`) and only in meetings with `record=true` and `allowStartStopRecording=true`. It
is active only in meetings created with `meta_recap-enabled=true`. The plugin sends no data outside BBB; the summary
comes from the recording through the exporter ([install-exporter.md](install-exporter.md)) as before.

You need BBB 4.0 (plugin SDK `1.0.0-beta.3`), a static HTTPS host and Node.js 24 or later to build. The plugin is
built and unit-tested but not yet tested on a BBB server.

`RecapPlugin.js` bundles the BBB plugin SDK (LGPL-3.0), React and browser-bunyan (MIT); [`plugin/NOTICE`](../../plugin/NOTICE)
lists versions, licenses and sources.

## Quick start

1. Build, on any machine with Node.js and git:

   ```sh
   VERSION=v0.1.0                              # same release as the service
   git clone --depth 1 --branch "$VERSION" https://github.com/virtUOS/bbb-recap.git
   cd bbb-recap/plugin
   npm ci && npm run build
   ls dist                                     # expect: RecapPlugin.js  manifest.json
   ```

2. Upload both files from `dist/` into one directory on a static HTTPS host and check that they are served:

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
   PLUGIN_URL=https://cdn.example.org/bbb-recap/v0.1.0      # from step 2
   PROPS=/etc/bigbluebutton/bbb-web.properties
   if grep -q '^pluginManifests=' "$PROPS"; then
     grep '^pluginManifests=' "$PROPS"          # already set: add {"url":"$PLUGIN_URL/manifest.json"} to this list
   else
     echo "pluginManifests=[{\"url\":\"$PLUGIN_URL/manifest.json\"}]" >> "$PROPS" && systemctl restart bbb-web
   fi
   ```

   **Docker BBB** (bigbluebutton PR #26186 setup; `BBB_DIR` holds its `docker-compose.yml`): bbb-web takes any key of
   its properties from the environment (PR #26186 @ `152e63c`, `docker/README.md`, "Environment variables"). If a
   `docker-compose.override.yml` exists already, add the `web` service to it instead.

   ```sh
   PLUGIN_URL=https://cdn.example.org/bbb-recap/v0.1.0      # from step 2
   cat > "$BBB_DIR/docker-compose.override.yml" <<EOF
   services:
     web:
       environment:
         PLUGINMANIFESTS: '[{"url":"$PLUGIN_URL/manifest.json"}]'
   EOF
   cd "$BBB_DIR" && docker compose up -d web
   ```

4. Check: create a meeting with `meta_recap-enabled=true`, `record=true` and `allowStartStopRecording=true` (a front
   end with recap does this) and join as moderator. The button "AI summary: off" appears right of the recording
   indicator. A viewer does not see it. In a meeting without `meta_recap-enabled` the plugin is not loaded; with any
   value other than `true` it loads but shows nothing.

## How the plugin is limited to recap meetings

`manifest.json` contains the placeholder `${meta_recap-enabled}` without a default. bbb-web fills it from the
meeting's `meta_recap-enabled` parameter and does not load the plugin when the parameter is missing; the client only
writes a debug log. bbb-web passes the value on in the script URL, and the plugin stays idle unless it is the literal
`true` ([metadata-interface.md](../reference/metadata-interface.md) §1.2). So the global `pluginManifests` entry is
enough: no change in the front end. Sources: BBB `docs/docs/plugins.md` ("Meta\_ parameters", "Default value
(fallback) for missing placeholder's parameters") and `bbb-common-web/.../api/util/PluginUtils.java` at BBB v4.0.x.

## Other ways to host

- **On the BBB server, packaged BBB:** copy both files to `/var/www/bigbluebutton-default/assets/plugins/recap/`; the
  manifest URL is then `https://$BBB_HOST/plugins/recap/manifest.json` (BBB `docs/docs/plugins.md`, "Hosting the
  Plugin on a BBB Server").
- **On the BBB server, docker BBB** (PR #26186 setup): its nginx serves `/var/www/bigbluebutton-default/assets` as
  well (`docker/compose/nginx/bigbluebutton`). Copy both files to `$BBB_DIR/plugins/recap/` and mount that directory
  in `docker-compose.override.yml`, then `docker compose up -d nginx`:

  ```yaml
  services:
    nginx:
      volumes:
        - ./plugins/recap:/var/www/bigbluebutton-default/assets/plugins/recap:ro,z
  ```

- **Per meeting instead of server-wide:** a front end can pass `pluginManifests=[{"url":"…/manifest.json"}]` on
  `create` of recap meetings only; BBB merges it with the server-wide list.

## Upgrade

Build the new release, upload it to a new directory (`$PLUGIN_URL` with the new version), change the URL in
`pluginManifests` and restart bbb-web as in step 3. Running meetings keep the old version. The manifest carries the plugin version, which BBB appends to the script URL, so browsers do not
use a cached old script. With `pluginManifestCacheEnabled=true`, bbb-web may keep an old manifest for up to
`pluginManifestCacheRefreshIntervalMinutes`.

The plugin uses BBB's recording GraphQL fields, which are not part of the plugin SDK's contract. Test it after every
BBB minor upgrade. `requiredSdkVersion` in the manifest (`^1.0.0-beta.3`) makes BBB refuse to load the plugin with an
incompatible SDK.

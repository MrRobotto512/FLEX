# FLEX for Android / Android TV

A full-screen WebView of the server's own web client (`web/`), so phones, tablets and Android TV boxes get the same Home screen, Movies and TV grids and player as a browser, launched from an icon. There is no UI of its own beyond finding the server:

- On the first start it broadcasts for FLEX servers like the PS5 does (`FLEX_DISCOVER` to UDP 7555), opens the one it finds or offers a choice, and remembers it along with its Tailscale address (used when the home address doesn't answer, i.e. away from home). An address can be typed instead (`192.168.0.205:8484`, `flex:8484`).
- **Hold Back** for the app's menu: search for FLEX PCs, enter an address, check for updates (asks GitHub for the latest release, so it works anywhere; the automatic offer when the server is newer stays). **Menu** switches Home → Movies → TV (the page's Tab); **holding Menu** opens *Cast to…* (the page's C); **Search** focuses the search box. Phones and tablets use the page's own buttons (Cast, Back, the player's control bar on a tap). Play/pause, forward and rewind keys on a remote are passed to the player; the D-pad and OK arrive as arrows and Enter, which the page already handles. Back closes the player or an episode list, and leaves the app from the home screen.
- When the server runs a newer FLEX than the app, it offers to download that release's `FLEX-android.apk` and install it (Android asks once to allow installs from FLEX). The page itself always comes from the server, so the app rarely needs updating.

## Building

CI builds it (`.github/workflows/android.yml` on changes, `release.yml` for releases, signed with the release key from the repository's secrets `ANDROID_KEYSTORE` (base64 PKCS12, alias `flex`) and `ANDROID_KEYSTORE_PASSWORD`). Locally, with a JDK 17 and the Android SDK:

```bash
cd android
gradle assembleRelease -PflexVersion=0.6.0   # or ./gradlew after `gradle wrapper`
```

Without `FLEX_KEYSTORE` / `FLEX_KEYSTORE_PASSWORD` in the environment the APK is signed with the debug key; such a build doesn't install over a release build (different signature): uninstall first.

Install on a TV box or phone: `adb install FLEX-android.apk`, or open the APK from a USB stick or the box's browser.

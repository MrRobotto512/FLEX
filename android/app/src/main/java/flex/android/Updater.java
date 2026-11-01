package flex.android;

import android.app.AlertDialog;
import android.app.DownloadManager;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.database.Cursor;
import android.net.Uri;
import android.widget.Toast;

import androidx.core.content.ContextCompat;
import androidx.core.content.FileProvider;

import org.json.JSONObject;

import java.io.File;
import java.io.InputStream;
import java.net.HttpURLConnection;
import java.net.URL;

/** Offers the app of a newer release once the server runs one: every FLEX
 *  release carries FLEX-android.apk, and the PCs update themselves first.
 *  The page inside is the server's own, so it is always current anyway. */
final class Updater {
    static final String REPO = "MrRobotto512/FLEX";
    private final MainActivity act;
    private BroadcastReceiver done;
    private long downloadId = -1;

    Updater(MainActivity act) {
        this.act = act;
    }

    /** Asks the server about itself off the main thread: its Tailscale address
     *  is kept for away from home, and a newer version is offered. */
    void check(String base) {
        new Thread(() -> {
            JSONObject s = serverInfo(base);
            if (s == null) return;
            JSONObject ts = s.optJSONObject("tailscale");
            String ip = ts != null ? ts.optString("ip", "") : "";
            int port = 8484;
            try {
                int p = new URL(base).getPort();
                if (p > 0) port = p;
            } catch (Exception ignored) {
            }
            String tsUrl = ip.isEmpty() ? null : "http://" + ip + ":" + port;
            String v = s.optString("version", "");
            act.runOnUiThread(() -> {
                act.rememberTailscale(tsUrl);
                if (!BuildConfig.VERSION_NAME.equals("0.0.0") && newer(v, BuildConfig.VERSION_NAME)) offer(v);
            });
        }).start();
    }

    void close() {
        if (done != null) {
            act.unregisterReceiver(done);
            done = null;
        }
    }

    private static JSONObject serverInfo(String base) {
        try {
            HttpURLConnection c = (HttpURLConnection) new URL(base + "/api/v1/server").openConnection();
            c.setConnectTimeout(5000);
            c.setReadTimeout(5000);
            c.setRequestProperty("X-Flex-Client", "android");
            try (InputStream in = c.getInputStream()) {
                byte[] b = new byte[64 * 1024];
                int n, len = 0;
                while (len < b.length && (n = in.read(b, len, b.length - len)) > 0) len += n;
                return new JSONObject(new String(b, 0, len, "UTF-8"));
            }
        } catch (Exception e) {
            return null;
        }
    }

    /** newer: a > b as x.y.z versions; false when either isn't one. */
    static boolean newer(String a, String b) {
        String[] x = a.split("\\."), y = b.split("\\.");
        if (x.length != 3 || y.length != 3) return false;
        try {
            for (int i = 0; i < 3; i++) {
                int d = Integer.parseInt(x[i]) - Integer.parseInt(y[i]);
                if (d != 0) return d > 0;
            }
        } catch (NumberFormatException e) {
            return false;
        }
        return false;
    }

    /** Check for updates from the menu: asks GitHub for the latest release
     *  rather than the server, so it works away from home and before the
     *  server has updated itself; "Later" is not remembered here. */
    void checkGitHub() {
        Toast.makeText(act, "Checking GitHub…", Toast.LENGTH_SHORT).show();
        new Thread(() -> {
            String v = latestRelease();
            act.runOnUiThread(() -> {
                if (v == null) Toast.makeText(act, "Couldn't reach GitHub.", Toast.LENGTH_LONG).show();
                else if (newer(v, BuildConfig.VERSION_NAME)) offer(v, true);
                else Toast.makeText(act, "FLEX " + BuildConfig.VERSION_NAME + " is the latest.", Toast.LENGTH_LONG).show();
            });
        }).start();
    }

    private static String latestRelease() {
        try {
            HttpURLConnection c = (HttpURLConnection) new URL("https://api.github.com/repos/" + REPO + "/releases/latest").openConnection();
            c.setConnectTimeout(8000);
            c.setReadTimeout(8000);
            c.setRequestProperty("Accept", "application/vnd.github+json");
            try (InputStream in = c.getInputStream()) {
                byte[] b = new byte[256 * 1024];
                int n, len = 0;
                while (len < b.length && (n = in.read(b, len, b.length - len)) > 0) len += n;
                String tag = new JSONObject(new String(b, 0, len, "UTF-8")).optString("tag_name", "");
                return tag.startsWith("v") ? tag.substring(1) : tag;
            }
        } catch (Exception e) {
            return null;
        }
    }

    private void offer(String v) {
        offer(v, false);
    }

    private void offer(String v, boolean force) {
        if (!force && v.equals(act.prefs().getString("skip", ""))) return;
        new AlertDialog.Builder(act)
                .setTitle("FLEX " + v + " is out")
                .setMessage("Download and install the new app? (This app is " + BuildConfig.VERSION_NAME + ".)")
                .setPositiveButton("Update", (d, w) -> download(v))
                .setNegativeButton("Later", (d, w) -> act.prefs().edit().putString("skip", v).apply())
                .show();
    }

    private void download(String v) {
        File dir = new File(act.getExternalFilesDir(null), "updates");
        dir.mkdirs();
        File apk = new File(dir, "FLEX-android.apk");
        apk.delete();
        Uri src = Uri.parse("https://github.com/" + REPO + "/releases/download/v" + v + "/FLEX-android.apk");
        DownloadManager dm = (DownloadManager) act.getSystemService(Context.DOWNLOAD_SERVICE);
        DownloadManager.Request req = new DownloadManager.Request(src)
                .setTitle("FLEX " + v)
                .setDestinationUri(Uri.fromFile(apk))
                .setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE);
        close();
        done = new BroadcastReceiver() {
            @Override public void onReceive(Context c, Intent i) {
                if (i.getLongExtra(DownloadManager.EXTRA_DOWNLOAD_ID, -1) != downloadId) return;
                close();
                if (finished(dm)) install(apk);
                else Toast.makeText(act, "The download failed; try again later.", Toast.LENGTH_LONG).show();
            }
        };
        ContextCompat.registerReceiver(act, done, new IntentFilter(DownloadManager.ACTION_DOWNLOAD_COMPLETE),
                ContextCompat.RECEIVER_EXPORTED);
        downloadId = dm.enqueue(req);
        Toast.makeText(act, "Downloading FLEX " + v + "…", Toast.LENGTH_SHORT).show();
    }

    private boolean finished(DownloadManager dm) {
        try (Cursor c = dm.query(new DownloadManager.Query().setFilterById(downloadId))) {
            return c.moveToFirst()
                    && c.getInt(c.getColumnIndexOrThrow(DownloadManager.COLUMN_STATUS)) == DownloadManager.STATUS_SUCCESSFUL;
        } catch (Exception e) {
            return false;
        }
    }

    private void install(File apk) {
        Uri uri = FileProvider.getUriForFile(act, "flex.android.files", apk);
        Intent i = new Intent(Intent.ACTION_VIEW)
                .setDataAndType(uri, "application/vnd.android.package-archive")
                .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION | Intent.FLAG_ACTIVITY_NEW_TASK);
        try {
            act.startActivity(i);
        } catch (Exception e) {
            Toast.makeText(act, "Couldn't open the installer: " + e.getMessage(), Toast.LENGTH_LONG).show();
        }
    }
}

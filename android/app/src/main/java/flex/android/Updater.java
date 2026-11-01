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

    /** Asks the server its version off the main thread; offers the update on it. */
    void check(String base) {
        if (BuildConfig.VERSION_NAME.equals("0.0.0")) return; // a dev build
        new Thread(() -> {
            String v = serverVersion(base);
            if (v != null && newer(v, BuildConfig.VERSION_NAME)) act.runOnUiThread(() -> offer(v));
        }).start();
    }

    void close() {
        if (done != null) {
            act.unregisterReceiver(done);
            done = null;
        }
    }

    private static String serverVersion(String base) {
        try {
            HttpURLConnection c = (HttpURLConnection) new URL(base + "/api/v1/server").openConnection();
            c.setConnectTimeout(5000);
            c.setReadTimeout(5000);
            c.setRequestProperty("X-Flex-Client", "android");
            try (InputStream in = c.getInputStream()) {
                byte[] b = new byte[64 * 1024];
                int n, len = 0;
                while (len < b.length && (n = in.read(b, len, b.length - len)) > 0) len += n;
                return new JSONObject(new String(b, 0, len, "UTF-8")).optString("version", "");
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

    private void offer(String v) {
        if (v.equals(act.prefs().getString("skip", ""))) return;
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

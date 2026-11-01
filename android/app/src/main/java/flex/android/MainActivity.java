package flex.android;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.SharedPreferences;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.text.InputType;
import android.view.KeyEvent;
import android.view.View;
import android.view.ViewGroup;
import android.view.WindowManager;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceError;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.Toast;

import org.json.JSONObject;

import java.util.List;

/** The whole app: the server's own web client, full screen. Finds the server
 *  like the PS5 does, remembers it and its Tailscale address for away from
 *  home, and turns the remote's keys into what the page expects. Hold Back
 *  to pick another server. */
public class MainActivity extends Activity {
    private static final int MATCH = ViewGroup.LayoutParams.MATCH_PARENT;

    private final Handler ui = new Handler(Looper.getMainLooper());
    private SharedPreferences prefs;
    private Updater updater;
    private FrameLayout root;
    private WebView web;
    private String base;            // the address being shown, e.g. http://192.168.0.205:8484
    private boolean triedTs;        // the Tailscale address was tried for this server
    private boolean backHeld;       // this Back press was a long one (server picker)
    private View fullVideo;         // the page's full-screen video, when it asks for one
    private WebChromeClient.CustomViewCallback fullVideoDone;

    SharedPreferences prefs() {
        return prefs;
    }

    @Override protected void onCreate(Bundle state) {
        super.onCreate(state);
        prefs = getSharedPreferences("flex", MODE_PRIVATE);
        updater = new Updater(this);
        root = new FrameLayout(this);
        root.setBackgroundColor(0xff0b0d12);
        web = new WebView(this);
        web.setBackgroundColor(0xff0b0d12);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setMediaPlaybackRequiresUserGesture(false);
        s.setUseWideViewPort(true);
        s.setLoadWithOverviewMode(true);
        s.setUserAgentString(s.getUserAgentString() + " FLEX-Android/" + BuildConfig.VERSION_NAME);
        web.setWebViewClient(new WebViewClient() {
            @Override public void onReceivedError(WebView v, WebResourceRequest req, WebResourceError err) {
                if (req.isForMainFrame()) ui.post(MainActivity.this::unreachable);
            }

            @SuppressWarnings("deprecation") // the one Android 5 and 6 call
            @Override public void onReceivedError(WebView v, int code, String desc, String url) {
                ui.post(MainActivity.this::unreachable);
            }

            @Override public void onPageFinished(WebView v, String url) {
                if (base != null && url.startsWith(base)) updater.check(base);
            }
        });
        web.setWebChromeClient(new WebChromeClient() {
            @Override public void onShowCustomView(View view, CustomViewCallback cb) {
                if (fullVideo != null) {
                    cb.onCustomViewHidden();
                    return;
                }
                fullVideo = view;
                fullVideoDone = cb;
                web.setVisibility(View.GONE);
                root.addView(view, new FrameLayout.LayoutParams(MATCH, MATCH));
            }

            @Override public void onHideCustomView() {
                if (fullVideo == null) return;
                root.removeView(fullVideo);
                fullVideo = null;
                fullVideoDone = null;
                web.setVisibility(View.VISIBLE);
                web.requestFocus();
            }
        });
        root.addView(web, new FrameLayout.LayoutParams(MATCH, MATCH));
        setContentView(root);
        getWindow().addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
        hideSystemBars();
        web.requestFocus();

        String saved = prefs.getString("url", null);
        if (saved != null) load(saved);
        else discover(false);
    }

    // --- finding the server ---

    private void load(String url) {
        base = url;
        triedTs = false;
        web.loadUrl(url + "/");
    }

    /** The page didn't load: try the Tailscale address once, then ask. */
    private void unreachable() {
        String ts = prefs.getString("ts", null);
        if (!triedTs && ts != null && !ts.equals(base)) {
            triedTs = true;
            base = ts;
            web.loadUrl(ts + "/");
            return;
        }
        String name = prefs.getString("name", base);
        new AlertDialog.Builder(this)
                .setTitle("Can't reach " + name)
                .setMessage("Is the PC with FLEX on, and is this device on the same network?")
                .setPositiveButton("Search again", (d, w) -> discover(true))
                .setNeutralButton("Enter address", (d, w) -> askAddress())
                .setNegativeButton("Retry", (d, w) -> load(prefs.getString("url", base)))
                .show();
    }

    /** Broadcasts for servers; one found goes straight in, more are offered. */
    private void discover(boolean manual) {
        Toast.makeText(this, "Looking for FLEX on the network…", Toast.LENGTH_SHORT).show();
        new Thread(() -> {
            List<Discovery.Found> found = Discovery.run(2000);
            ui.post(() -> {
                if (found.isEmpty()) {
                    new AlertDialog.Builder(this)
                            .setTitle("No FLEX server found")
                            .setMessage("Is the PC with FLEX on, and is this device on the same network?")
                            .setPositiveButton("Search again", (d, w) -> discover(manual))
                            .setNegativeButton("Enter address", (d, w) -> askAddress())
                            .show();
                } else if (found.size() == 1 && !manual) {
                    choose(found.get(0));
                } else {
                    String[] names = new String[found.size() + 1];
                    for (int i = 0; i < found.size(); i++) names[i] = found.get(i).toString();
                    names[found.size()] = "Enter an address…";
                    new AlertDialog.Builder(this)
                            .setTitle("Which FLEX?")
                            .setItems(names, (d, i) -> {
                                if (i < found.size()) choose(found.get(i));
                                else askAddress();
                            })
                            .show();
                }
            });
        }).start();
    }

    private void choose(Discovery.Found f) {
        prefs.edit().putString("url", f.url()).putString("ts", f.tsUrl()).putString("name", f.name).apply();
        load(f.url());
    }

    private void askAddress() {
        EditText in = new EditText(this);
        in.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_URI);
        in.setHint("192.168.0.205:8484 or flex:8484");
        String cur = prefs.getString("url", "");
        in.setText(cur.replaceFirst("^https?://", ""));
        new AlertDialog.Builder(this)
                .setTitle("FLEX server address")
                .setView(in)
                .setPositiveButton("Open", (d, w) -> {
                    String a = in.getText().toString().trim().replaceFirst("/+$", "");
                    if (a.isEmpty()) {
                        discover(true);
                        return;
                    }
                    if (!a.startsWith("http://") && !a.startsWith("https://")) a = "http://" + a;
                    if (!a.matches(".*:\\d+$")) a += ":8484";
                    prefs.edit().putString("url", a).remove("ts").putString("name", a).apply();
                    load(a);
                })
                .setNegativeButton("Search", (d, w) -> discover(true))
                .show();
    }

    // --- keys: the page handles arrows and Enter itself; the rest is mapped here ---

    @Override public boolean dispatchKeyEvent(KeyEvent e) {
        boolean down = e.getAction() == KeyEvent.ACTION_DOWN;
        switch (e.getKeyCode()) {
            case KeyEvent.KEYCODE_BACK:
                if (fullVideo != null) {
                    if (down && fullVideoDone != null) fullVideoDone.onCustomViewHidden();
                    return true;
                }
                if (down) {
                    if (e.getRepeatCount() == 0) backHeld = false;
                    else if ((e.getFlags() & KeyEvent.FLAG_LONG_PRESS) != 0 && !backHeld) {
                        backHeld = true;
                        discover(true);
                    }
                } else if (!backHeld) {
                    back();
                }
                return true;
            case KeyEvent.KEYCODE_MENU:
                if (down) sendKey("Tab"); // Home → Movies → TV
                return true;
            case KeyEvent.KEYCODE_MEDIA_PLAY_PAUSE:
            case KeyEvent.KEYCODE_MEDIA_PLAY:
            case KeyEvent.KEYCODE_MEDIA_PAUSE:
                if (down) sendKey(" ");
                return true;
            case KeyEvent.KEYCODE_MEDIA_FAST_FORWARD:
                if (down) sendKey("ArrowRight");
                return true;
            case KeyEvent.KEYCODE_MEDIA_REWIND:
                if (down) sendKey("ArrowLeft");
                return true;
            case KeyEvent.KEYCODE_MEDIA_STOP:
                if (down) sendKey("Escape");
                return true;
            default:
                return super.dispatchKeyEvent(e);
        }
    }

    /** Back closes what the page has open; at the home screen it leaves the app. */
    private void back() {
        web.evaluateJavascript("typeof flexBack === 'function' ? String(flexBack()) : 'false'", r -> {
            if (!"\"true\"".equals(r)) finish();
        });
    }

    private void sendKey(String key) {
        web.evaluateJavascript("document.dispatchEvent(new KeyboardEvent('keydown', {key: "
                + JSONObject.quote(key) + ", bubbles: true, cancelable: true}))", null);
    }

    // --- housekeeping ---

    @SuppressWarnings("deprecation") // the flags work on every Android this runs on
    private void hideSystemBars() {
        getWindow().getDecorView().setSystemUiVisibility(View.SYSTEM_UI_FLAG_IMMERSIVE_STICKY
                | View.SYSTEM_UI_FLAG_HIDE_NAVIGATION | View.SYSTEM_UI_FLAG_FULLSCREEN
                | View.SYSTEM_UI_FLAG_LAYOUT_HIDE_NAVIGATION | View.SYSTEM_UI_FLAG_LAYOUT_FULLSCREEN
                | View.SYSTEM_UI_FLAG_LAYOUT_STABLE);
    }

    @Override public void onWindowFocusChanged(boolean has) {
        super.onWindowFocusChanged(has);
        if (has) hideSystemBars();
    }

    @Override protected void onPause() {
        super.onPause();
        web.onPause();
    }

    @Override protected void onResume() {
        super.onResume();
        web.onResume();
    }

    @Override protected void onDestroy() {
        updater.close();
        root.removeView(web);
        web.destroy();
        super.onDestroy();
    }
}

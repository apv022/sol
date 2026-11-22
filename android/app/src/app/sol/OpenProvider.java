package app.sol;

import android.content.ContentProvider;
import android.content.ContentValues;
import android.content.Context;
import android.database.Cursor;
import android.database.MatrixCursor;
import android.net.Uri;
import android.os.ParcelFileDescriptor;
import android.provider.OpenableColumns;
import android.webkit.MimeTypeMap;

import java.io.File;
import java.io.FileNotFoundException;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;

/**
 * Serves the files the shell fetched to open in another app (a PDF from Files, opened by the
 * system viewer) as content://app.sol.open/&lt;key&gt;/&lt;name&gt;, read-only.
 *
 * Invariants: it only ever serves files under cacheDir/open/ (a path that resolves elsewhere is
 * not found), it is not exported, and a viewer reaches a file only through the per-intent
 * FLAG_GRANT_READ_URI_PERMISSION the activity adds. It is the minimal FileProvider an
 * AndroidX-free build can have.
 */
public class OpenProvider extends ContentProvider {
    /** The provider's authority (matches the manifest). */
    static final String AUTHORITY = "app.sol.open";
    /** Cached files older than this are deleted when a new one is fetched. */
    private static final long KEEP_MS = 24L * 60 * 60 * 1000;

    /** The directory every served file lives in. */
    static File dir(Context c) { return new File(c.getCacheDir(), "open"); }

    /** A stable cache key (hex SHA-256) for a fetched URL and its validators. */
    static String key(String s) {
        try {
            byte[] h = MessageDigest.getInstance("SHA-256").digest(s.getBytes(StandardCharsets.UTF_8));
            StringBuilder b = new StringBuilder();
            for (int i = 0; i < 16; i++) b.append(String.format("%02x", h[i]));
            return b.toString();
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException(e);
        }
    }

    /** Where the file for key and name is cached. */
    static File file(Context c, String key, String name) { return new File(new File(dir(c), key), name); }

    /** The content:// URI that serves f (a file from {@link #file}). */
    static Uri uri(File f) {
        return new Uri.Builder().scheme("content").authority(AUTHORITY)
                .appendPath(f.getParentFile().getName()).appendPath(f.getName()).build();
    }

    /** Deletes cached files not used for a day, so the cache does not grow forever. */
    static void prune(Context c) {
        File[] keys = dir(c).listFiles();
        if (keys == null) return;
        long cutoff = System.currentTimeMillis() - KEEP_MS;
        for (File k : keys) {
            File[] fs = k.listFiles();
            boolean old = k.lastModified() < cutoff;
            if (fs != null) for (File f : fs) if (f.lastModified() < cutoff || f.getName().endsWith(".part")) f.delete(); else old = false;
            if (old) k.delete();
        }
    }

    private File resolve(Uri u) throws FileNotFoundException {
        java.util.List<String> p = u.getPathSegments();
        if (p.size() != 2) throw new FileNotFoundException();
        try {
            File base = dir(getContext()).getCanonicalFile();
            File f = new File(new File(base, p.get(0)), p.get(1)).getCanonicalFile();
            File parent = f.getParentFile();
            if (parent == null || !base.equals(parent.getParentFile()) || !f.isFile()) throw new FileNotFoundException();
            return f;
        } catch (IOException e) {
            throw new FileNotFoundException();
        }
    }

    @Override public boolean onCreate() { return true; }

    @Override
    public ParcelFileDescriptor openFile(Uri u, String mode) throws FileNotFoundException {
        if (!"r".equals(mode)) throw new SecurityException("read-only");
        return ParcelFileDescriptor.open(resolve(u), ParcelFileDescriptor.MODE_READ_ONLY);
    }

    @Override
    public Cursor query(Uri u, String[] projection, String sel, String[] args, String sort) {
        File f;
        try { f = resolve(u); } catch (FileNotFoundException e) { return null; }
        String[] cols = projection != null ? projection : new String[]{OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE};
        MatrixCursor c = new MatrixCursor(cols, 1);
        Object[] row = new Object[cols.length];
        for (int i = 0; i < cols.length; i++) {
            if (OpenableColumns.DISPLAY_NAME.equals(cols[i])) row[i] = f.getName();
            else if (OpenableColumns.SIZE.equals(cols[i])) row[i] = f.length();
        }
        c.addRow(row);
        return c;
    }

    @Override
    public String getType(Uri u) {
        String n = u.getLastPathSegment();
        int dot = n == null ? -1 : n.lastIndexOf('.');
        String t = dot < 0 ? null : MimeTypeMap.getSingleton().getMimeTypeFromExtension(n.substring(dot + 1).toLowerCase(java.util.Locale.ROOT));
        return t != null ? t : "application/octet-stream";
    }

    @Override public Uri insert(Uri u, ContentValues v) { throw new UnsupportedOperationException("read-only"); }
    @Override public int delete(Uri u, String s, String[] a) { throw new UnsupportedOperationException("read-only"); }
    @Override public int update(Uri u, ContentValues v, String s, String[] a) { throw new UnsupportedOperationException("read-only"); }
}

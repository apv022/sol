package app.sol;

import android.database.Cursor;
import android.database.MatrixCursor;
import android.os.CancellationSignal;
import android.os.ParcelFileDescriptor;
import android.provider.DocumentsContract;
import android.provider.DocumentsContract.Document;
import android.provider.DocumentsContract.Root;
import android.provider.DocumentsProvider;
import android.webkit.MimeTypeMap;

import java.io.File;
import java.io.FileNotFoundException;
import java.io.IOException;
import java.util.Locale;

/**
 * The synced folder (SolDrive, the node's --files directory) as a "Sol" location in the system
 * Files app and every app's file picker, read and write. The node watches the folder, so what
 * another app writes here syncs like any other change.
 *
 * Document IDs are "drive:" plus the path relative to SolDrive ("drive:" is the folder itself).
 * Invariants: every ID resolves inside SolDrive (no "..", canonical path checked), and the
 * node's internal .sol folder is neither listed nor reachable. Only the system (holders of
 * MANAGE_DOCUMENTS) binds to it; other apps get per-document grants from the picker.
 */
public class DriveProvider extends DocumentsProvider {
    /** The provider's authority (matches the manifest). */
    static final String AUTHORITY = "app.sol.documents";
    private static final String ROOT = "sol";
    private static final String PREFIX = "drive:";
    private static final String INTERNAL = ".sol";

    private static final String[] ROOT_COLS = {Root.COLUMN_ROOT_ID, Root.COLUMN_FLAGS, Root.COLUMN_TITLE,
            Root.COLUMN_SUMMARY, Root.COLUMN_DOCUMENT_ID, Root.COLUMN_ICON, Root.COLUMN_AVAILABLE_BYTES, Root.COLUMN_MIME_TYPES};
    private static final String[] DOC_COLS = {Document.COLUMN_DOCUMENT_ID, Document.COLUMN_DISPLAY_NAME,
            Document.COLUMN_MIME_TYPE, Document.COLUMN_SIZE, Document.COLUMN_LAST_MODIFIED, Document.COLUMN_FLAGS};

    @Override public boolean onCreate() { return true; }

    private File drive() { return NodeService.driveDir(getContext()); }

    /** The file for a document ID, refusing anything outside SolDrive or inside .sol. */
    private File file(String id) throws FileNotFoundException {
        if (id == null || !id.startsWith(PREFIX)) throw new FileNotFoundException(id);
        String rel = id.substring(PREFIX.length());
        for (String part : rel.split("/")) {
            if (part.equals("..") || part.equals(".") || part.equals(INTERNAL)) throw new FileNotFoundException(id);
        }
        try {
            File base = drive().getCanonicalFile();
            File f = rel.isEmpty() ? base : new File(base, rel).getCanonicalFile();
            if (!f.equals(base) && !f.getPath().startsWith(base.getPath() + File.separator)) throw new FileNotFoundException(id);
            String inner = f.getPath().substring(base.getPath().length());
            if (inner.equals(File.separator + INTERNAL) || inner.startsWith(File.separator + INTERNAL + File.separator))
                throw new FileNotFoundException(id);
            return f;
        } catch (IOException e) {
            throw new FileNotFoundException(id);
        }
    }

    private String id(File f) throws FileNotFoundException {
        try {
            String base = drive().getCanonicalPath(), p = f.getCanonicalPath();
            if (p.equals(base)) return PREFIX;
            if (!p.startsWith(base + File.separator)) throw new FileNotFoundException();
            return PREFIX + p.substring(base.length() + 1);
        } catch (IOException e) {
            throw new FileNotFoundException();
        }
    }

    private static String parentId(String id) {
        int i = id.lastIndexOf('/');
        return i < 0 ? PREFIX : id.substring(0, i);
    }

    private static String mime(File f) {
        if (f.isDirectory()) return Document.MIME_TYPE_DIR;
        String n = f.getName();
        int dot = n.lastIndexOf('.');
        String t = dot < 0 ? null : MimeTypeMap.getSingleton().getMimeTypeFromExtension(n.substring(dot + 1).toLowerCase(Locale.ROOT));
        return t != null ? t : "application/octet-stream";
    }

    private void row(MatrixCursor c, File f) throws FileNotFoundException {
        String id = id(f);
        int flags;
        if (f.isDirectory()) {
            flags = Document.FLAG_DIR_SUPPORTS_CREATE;
            if (!id.equals(PREFIX)) flags |= Document.FLAG_SUPPORTS_DELETE | Document.FLAG_SUPPORTS_RENAME;
        } else {
            flags = Document.FLAG_SUPPORTS_WRITE | Document.FLAG_SUPPORTS_DELETE | Document.FLAG_SUPPORTS_RENAME;
        }
        c.newRow().add(Document.COLUMN_DOCUMENT_ID, id)
                .add(Document.COLUMN_DISPLAY_NAME, id.equals(PREFIX) ? "Sol" : f.getName())
                .add(Document.COLUMN_MIME_TYPE, mime(f))
                .add(Document.COLUMN_SIZE, f.isDirectory() ? null : f.length())
                .add(Document.COLUMN_LAST_MODIFIED, f.lastModified())
                .add(Document.COLUMN_FLAGS, flags);
    }

    @Override
    public Cursor queryRoots(String[] projection) {
        MatrixCursor c = new MatrixCursor(projection != null ? projection : ROOT_COLS);
        if (!NodeService.initialised(getContext())) return c; // no Sol yet: no location
        drive().mkdirs();
        c.newRow().add(Root.COLUMN_ROOT_ID, ROOT)
                .add(Root.COLUMN_FLAGS, Root.FLAG_SUPPORTS_CREATE | Root.FLAG_SUPPORTS_IS_CHILD | Root.FLAG_LOCAL_ONLY)
                .add(Root.COLUMN_TITLE, "Sol")
                .add(Root.COLUMN_SUMMARY, "Synced with your devices")
                .add(Root.COLUMN_DOCUMENT_ID, PREFIX)
                .add(Root.COLUMN_ICON, R.mipmap.ic_launcher)
                .add(Root.COLUMN_AVAILABLE_BYTES, drive().getUsableSpace())
                .add(Root.COLUMN_MIME_TYPES, "*/*");
        return c;
    }

    @Override
    public Cursor queryDocument(String id, String[] projection) throws FileNotFoundException {
        File f = file(id);
        if (!f.exists()) throw new FileNotFoundException(id);
        MatrixCursor c = new MatrixCursor(projection != null ? projection : DOC_COLS);
        row(c, f);
        return c;
    }

    @Override
    public Cursor queryChildDocuments(String parent, String[] projection, String sort) throws FileNotFoundException {
        File dir = file(parent);
        MatrixCursor c = new MatrixCursor(projection != null ? projection : DOC_COLS);
        File[] kids = dir.listFiles();
        if (kids != null) for (File k : kids) {
            if (parent.equals(PREFIX) && k.getName().equals(INTERNAL)) continue;
            row(c, k);
        }
        c.setNotificationUri(getContext().getContentResolver(), DocumentsContract.buildChildDocumentsUri(AUTHORITY, parent));
        return c;
    }

    @Override
    public ParcelFileDescriptor openDocument(String id, String mode, CancellationSignal signal) throws FileNotFoundException {
        File f = file(id);
        if (f.isDirectory()) throw new FileNotFoundException(id);
        int m = ParcelFileDescriptor.parseMode(mode);
        ParcelFileDescriptor pfd = ParcelFileDescriptor.open(f, m);
        if (!mode.equals("r")) changed(parentId(id));
        return pfd;
    }

    @Override
    public String createDocument(String parent, String mimeType, String displayName) throws FileNotFoundException {
        File dir = file(parent);
        if (!dir.isDirectory()) throw new FileNotFoundException(parent);
        String name = clean(displayName);
        if (parent.equals(PREFIX) && name.equals(INTERNAL)) throw new FileNotFoundException(name);
        boolean folder = Document.MIME_TYPE_DIR.equals(mimeType);
        File f = unique(dir, name, folder);
        try {
            if (folder ? !f.mkdir() : !f.createNewFile()) throw new FileNotFoundException(name);
        } catch (IOException e) {
            throw new FileNotFoundException(name);
        }
        changed(parent);
        return id(f);
    }

    @Override
    public String renameDocument(String id, String displayName) throws FileNotFoundException {
        File f = file(id);
        if (id.equals(PREFIX)) throw new FileNotFoundException(id);
        String name = clean(displayName);
        if (parentId(id).equals(PREFIX) && name.equals(INTERNAL)) throw new FileNotFoundException(name);
        File to = new File(f.getParentFile(), name);
        if (to.exists() || !f.renameTo(to)) throw new IllegalStateException("cannot rename to " + name);
        changed(parentId(id));
        return id(to);
    }

    @Override
    public void deleteDocument(String id) throws FileNotFoundException {
        File f = file(id);
        if (id.equals(PREFIX)) throw new FileNotFoundException(id);
        if (!deleteTree(f)) throw new IllegalStateException("cannot delete");
        changed(parentId(id));
    }

    @Override
    public String getDocumentType(String id) throws FileNotFoundException { return mime(file(id)); }

    @Override
    public boolean isChildDocument(String parent, String id) {
        return id.startsWith(PREFIX) && parent.startsWith(PREFIX)
                && (parent.equals(PREFIX) || id.startsWith(parent + "/")) && !id.equals(parent);
    }

    private void changed(String parent) {
        getContext().getContentResolver().notifyChange(DocumentsContract.buildChildDocumentsUri(AUTHORITY, parent), null);
    }

    private static boolean deleteTree(File f) {
        File[] kids = f.isDirectory() ? f.listFiles() : null;
        if (kids != null) for (File k : kids) deleteTree(k);
        return f.delete();
    }

    /** A display name usable as one path element. */
    private static String clean(String n) {
        n = n == null ? "" : n.replace('/', '_').replace('\0', '_').trim();
        if (n.isEmpty() || n.equals(".") || n.equals("..")) throw new IllegalArgumentException("bad name");
        return n;
    }

    /** dir/name, or "name (2).ext" when taken: creating never overwrites. */
    private static File unique(File dir, String name, boolean folder) {
        File f = new File(dir, name);
        int dot = folder ? -1 : name.lastIndexOf('.');
        String base = dot > 0 ? name.substring(0, dot) : name, ext = dot > 0 ? name.substring(dot) : "";
        for (int i = 2; f.exists(); i++) f = new File(dir, base + " (" + i + ")" + ext);
        return f;
    }
}

package codegraph.probe;

import java.io.InputStream;
import java.net.URL;
import java.nio.ByteBuffer;
import java.nio.channels.FileChannel;
import java.nio.file.Files;
import java.nio.file.LinkOption;
import java.nio.file.Path;
import java.nio.file.FileAlreadyExistsException;
import java.nio.file.StandardCopyOption;
import java.nio.file.StandardOpenOption;
import java.nio.file.attribute.PosixFilePermission;
import java.security.MessageDigest;
import java.time.Instant;
import java.util.HexFormat;
import java.util.Set;
import java.util.regex.Pattern;
import org.jetbrains.kotlin.compiler.plugin.CompilerPluginRegistrar;
import org.jetbrains.kotlin.config.CompilerConfiguration;

public final class CompilerIdentityProbe extends CompilerPluginRegistrar {
    public static final String VERSION = "2.0.0";
    public static final int PROTOCOL = 2;
    private static final String COMPILER_CLASS = "org.jetbrains.kotlin.cli.jvm.K2JVMCompiler";
    private static final Pattern TOKEN = Pattern.compile("[0-9a-f]{64}");
    private static final Pattern PROJECT = Pattern.compile("(?::[A-Za-z0-9_.-]+)*:?");
    private static final Pattern IDENTIFIER = Pattern.compile("[A-Za-z][A-Za-z0-9_]{0,127}");
    private static final Pattern TASK = Pattern.compile("(?::[A-Za-z0-9_.-]{1,128}){1,8}");
    private static final long MAX_CLASS_BYTES = 128L * 1024 * 1024;
    private static final long MAX_JAR_BYTES = 1024L * 1024 * 1024;

    @Override public String getPluginId() { return ProbeCommandLineProcessor.PLUGIN_ID; }
    @Override public boolean getSupportsK2() { return true; }

    @Override
    public void registerExtensions(ExtensionStorage extensions, CompilerConfiguration configuration) {
        try {
            observe(configuration);
        } catch (Exception ignored) {
            // Observation failure must not alter compilation results or diagnostics.
        }
    }

    private void observe(CompilerConfiguration configuration) throws Exception {
        String token = required(configuration, "token");
        String recordText = required(configuration, "record");
        String project = required(configuration, "project");
        String compilation = required(configuration, "compilation");
        String task = required(configuration, "task");
        String strategy = required(configuration, "strategy");
        if (!TOKEN.matcher(token).matches() || !PROJECT.matcher(project).matches()
                || !IDENTIFIER.matcher(compilation).matches() || !TASK.matcher(task).matches()) {
            throw new IllegalArgumentException("invalid CodeGraph compiler probe correlation values");
        }

        try {
            String compilerClassName = "";
            String compilerLoader = "";
            String compilerClassHash = "";
            String compilerSourceName = "unavailable";
            String compilerSourceHash = "";
            try {
                Class<?> compiler = Class.forName(COMPILER_CLASS, false, getClass().getClassLoader());
                compilerClassName = compiler.getName();
                compilerLoader = compiler.getClassLoader() == null ? "bootstrap" : compiler.getClassLoader().getClass().getName();
                compilerClassHash = hashResource(compiler, "K2JVMCompiler.class", MAX_CLASS_BYTES);
                URL compilerSource = compiler.getProtectionDomain().getCodeSource() == null
                        ? null : compiler.getProtectionDomain().getCodeSource().getLocation();
                compilerSourceName = safeBasename(compilerSource);
                compilerSourceHash = hashSource(compilerSource, MAX_JAR_BYTES);
            } catch (Exception unavailable) {
                // Keep the callback diagnostic-only when an identity input is unavailable.
            }

            String pluginJarHash = "";
            try {
                URL pluginSource = getClass().getProtectionDomain().getCodeSource() == null
                        ? null : getClass().getProtectionDomain().getCodeSource().getLocation();
                pluginJarHash = hashSource(pluginSource, MAX_JAR_BYTES);
            } catch (Exception unavailable) { }

            String runtimeVendor = safeRuntimeProperty("java.vendor");

            Path record = Path.of(recordText).toAbsolutePath().normalize();
            String json = "{"
                    + field("schema", "codegraph.kotlin_compiler_probe/v2") + ","
                    + "\"protocol_version\":" + PROTOCOL + ","
                    + field("status", "CALLBACK_OBSERVED") + ","
                    + field("invocation_token", token) + ","
                    + field("probe_version", VERSION) + ","
                    + field("probe_jar_sha256", pluginJarHash) + ","
                    + field("compiler_class", compilerClassName) + ","
                    + field("compiler_classloader", compilerLoader) + ","
                    + field("compiler_codesource_name", compilerSourceName) + ","
                    + field("compiler_codesource_sha256", compilerSourceHash) + ","
                    + field("compiler_class_resource_sha256", compilerClassHash) + ","
                    + field("project", project) + ","
                    + field("compilation", compilation) + ","
                    + field("task", task) + ","
                    + field("requested_execution_strategy", strategy) + ","
                    + "\"callback_executed\":true,"
                    + "\"process_id\":" + ProcessHandle.current().pid() + ","
                    + field("process_start", ProcessHandle.current().info().startInstant().map(Instant::toString).orElse("unavailable")) + ","
                    + field("runtime_version", boundedRuntimeVersion())
                    + "," + field("runtime_vendor", runtimeVendor)
                    + "}";
            writeAtomicNew(record, json.getBytes(java.nio.charset.StandardCharsets.UTF_8));
        } catch (Exception failure) {
            throw new IllegalStateException("CodeGraph compiler identity observation failed", failure);
        }
    }

    private static String required(CompilerConfiguration configuration, String name) {
        String value = configuration.get(ProbeCommandLineProcessor.KEYS.forName(name));
        if (value == null || value.isEmpty() || value.length() > 4096) {
            throw new IllegalArgumentException("missing or oversized CodeGraph compiler probe option");
        }
        return value;
    }

    private static String boundedRuntimeVersion() {
        return safeRuntimeProperty("java.version");
    }

    private static String safeRuntimeProperty(String name) {
        String value = System.getProperty(name, "");
        return value.matches("[A-Za-z0-9._+ -]{1,64}") ? value : "unknown";
    }

    private static String safeBasename(URL source) {
        if (source == null || !"file".equals(source.getProtocol())) return "unavailable";
        try {
            String name = Path.of(source.toURI()).getFileName().toString();
            return name.matches("[A-Za-z0-9._+-]{1,128}") ? name : "unavailable";
        } catch (Exception ignored) {
            return "unavailable";
        }
    }

    private static String hashSource(URL source, long limit) throws Exception {
        if (source == null || !"file".equals(source.getProtocol())) return "";
        Path path = Path.of(source.toURI());
        if (!Files.isRegularFile(path) || Files.size(path) > limit) return "";
        return hash(Files.newInputStream(path), limit);
    }

    private static String hashResource(Class<?> owner, String resource, long limit) throws Exception {
        try (InputStream input = owner.getResourceAsStream(resource)) {
            if (input == null) return "";
            return hash(input, limit);
        }
    }

    private static String hash(InputStream input, long limit) throws Exception {
        try (InputStream in = input) {
            MessageDigest digest = MessageDigest.getInstance("SHA-256");
            byte[] buffer = new byte[32768];
            long total = 0;
            int read;
            while ((read = in.read(buffer)) != -1) {
                total += read;
                if (total > limit) throw new IllegalArgumentException("identity input exceeds size limit");
                digest.update(buffer, 0, read);
            }
            return HexFormat.of().formatHex(digest.digest());
        }
    }

    private static void writeAtomicNew(Path destination, byte[] bytes) throws Exception {
        if (bytes.length > 65536 || Files.exists(destination, LinkOption.NOFOLLOW_LINKS)) {
            throw new IllegalStateException("probe record is oversized or already exists");
        }
        Path parent = destination.getParent();
        if (parent == null || !Files.isDirectory(parent)) throw new IllegalStateException("probe record directory is unavailable");
        Path lock = destination.resolveSibling(destination.getFileName() + ".lock");
        try {
            Files.createFile(lock);
        } catch (FileAlreadyExistsException duplicate) {
            Path marker = destination.resolveSibling(destination.getFileName() + ".duplicate");
            Files.createFile(marker);
            try { Files.setPosixFilePermissions(marker, Set.of(PosixFilePermission.OWNER_READ, PosixFilePermission.OWNER_WRITE)); }
            catch (UnsupportedOperationException ignored) { }
            return;
        }
        try { Files.setPosixFilePermissions(lock, Set.of(PosixFilePermission.OWNER_READ, PosixFilePermission.OWNER_WRITE)); }
        catch (UnsupportedOperationException ignored) { }
        Path temporary = Files.createTempFile(parent, ".cg35-probe-", ".tmp");
        try {
            try { Files.setPosixFilePermissions(temporary, Set.of(PosixFilePermission.OWNER_READ, PosixFilePermission.OWNER_WRITE)); }
            catch (UnsupportedOperationException ignored) { }
            try (FileChannel channel = FileChannel.open(temporary, StandardOpenOption.WRITE, StandardOpenOption.TRUNCATE_EXISTING)) {
                ByteBuffer buffer = ByteBuffer.wrap(bytes);
                while (buffer.hasRemaining()) channel.write(buffer);
                channel.force(true);
            }
            // The complete, fsynced file becomes visible in one atomic directory
            // operation. createLink also guarantees that an existing target is
            // never replaced, unlike implementation-specific atomic move rules.
            Files.createLink(destination, temporary);
        } finally {
            Files.deleteIfExists(temporary);
        }
    }

    private static String field(String name, String value) {
        return "\"" + name + "\":\"" + value.replace("\\", "\\\\").replace("\"", "\\\"") + "\"";
    }
}

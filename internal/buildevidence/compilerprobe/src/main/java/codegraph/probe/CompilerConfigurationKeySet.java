package codegraph.probe;

import java.util.Map;
import org.jetbrains.kotlin.config.CompilerConfigurationKey;

final class CompilerConfigurationKeySet {
    private final Map<String, CompilerConfigurationKey<String>> keys = Map.of(
            "token", CompilerConfigurationKey.create("cg35 probe token"),
            "record", CompilerConfigurationKey.create("cg35 probe record"),
            "project", CompilerConfigurationKey.create("cg35 probe project"),
            "compilation", CompilerConfigurationKey.create("cg35 probe compilation"),
            "task", CompilerConfigurationKey.create("cg35 probe task"),
            "strategy", CompilerConfigurationKey.create("cg35 probe execution strategy"));

    CompilerConfigurationKey<String> forName(String name) {
        CompilerConfigurationKey<String> key = keys.get(name);
        if (key == null) throw new IllegalArgumentException("unknown CodeGraph compiler probe option");
        return key;
    }
}

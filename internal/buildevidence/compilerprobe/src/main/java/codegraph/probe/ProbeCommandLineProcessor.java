package codegraph.probe;

import java.util.List;
import org.jetbrains.kotlin.compiler.plugin.AbstractCliOption;
import org.jetbrains.kotlin.compiler.plugin.CliOption;
import org.jetbrains.kotlin.compiler.plugin.CommandLineProcessor;
import org.jetbrains.kotlin.config.CompilerConfiguration;

public final class ProbeCommandLineProcessor implements CommandLineProcessor {
    static final String PLUGIN_ID = "codegraph.compiler.identity";
    static final CompilerConfigurationKeySet KEYS = new CompilerConfigurationKeySet();
    private static final List<AbstractCliOption> OPTIONS = List.of(
            option("token", true), option("record", true), option("project", true),
            option("compilation", true), option("task", true), option("strategy", true));

    private static CliOption option(String name, boolean required) {
        return new CliOption(name, "value", "CodeGraph diagnostic probe " + name,
                required, false);
    }

    @Override public String getPluginId() { return PLUGIN_ID; }
    @Override public List<AbstractCliOption> getPluginOptions() { return OPTIONS; }

    @Override
    public void processOption(AbstractCliOption option, String value, CompilerConfiguration configuration) {
        if (!OPTIONS.contains(option) || value == null || value.isEmpty()) {
            throw new IllegalArgumentException("invalid CodeGraph compiler probe option");
        }
        configuration.put(KEYS.forName(option.getOptionName()), value);
    }
}

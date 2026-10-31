package sample;

public final class Caller {
    public String call(Api api) { return api.echo("ok"); }
    public String callTop(String value) { return Functions.top(value); }
}

from lib import full


def run():
    g = lambda x: lambda full: full()
    return g(0)(lambda: "inner")

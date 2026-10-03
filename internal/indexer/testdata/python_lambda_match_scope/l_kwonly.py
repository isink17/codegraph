from lib import full


def run():
    g = lambda *, full: full()
    return g(full=lambda: "kwonly")

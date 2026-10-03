def café():
    return "module"


def make():
    return lambda: "local"


def run():
    (café := make())
    return café()

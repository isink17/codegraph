def café():
    return "module"


def make():
    return lambda: "local"


def run():
    for café in [make()]:
        return café()

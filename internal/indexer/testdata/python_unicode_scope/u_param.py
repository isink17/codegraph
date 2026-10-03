def café():
    return "module"


def run(café=lambda: "local"):
    return café()

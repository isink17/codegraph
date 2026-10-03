from lib import ve


def make():
    return lambda: "local"


def run():
    (naïve := make())
    return ve()

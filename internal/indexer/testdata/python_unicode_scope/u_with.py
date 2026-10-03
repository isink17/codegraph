import contextlib


def café():
    return "module"


def make():
    return lambda: "local"


def run():
    with contextlib.nullcontext(make()) as café:
        return café()

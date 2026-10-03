from lib import full


def run(cb=lambda: full()):
    return cb()

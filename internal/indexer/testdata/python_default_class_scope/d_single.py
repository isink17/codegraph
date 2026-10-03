from lib import full


def run(cb=lambda full: full()):
    return cb(lambda: "param")

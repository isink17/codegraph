from lib import full


def run():
    def inner(cb=lambda full: full()):
        return cb(lambda: "param")
    return inner()

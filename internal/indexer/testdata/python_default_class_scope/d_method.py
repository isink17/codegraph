from lib import full


class K:
    def go(self, cb=lambda full: full()):
        return cb(lambda: "param")


def run():
    return K().go()

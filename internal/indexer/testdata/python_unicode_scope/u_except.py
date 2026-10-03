class Boom(Exception):
    def __call__(self):
        return "local"


def café():
    return "module"


def run():
    try:
        raise Boom()
    except Boom as café:
        return café()

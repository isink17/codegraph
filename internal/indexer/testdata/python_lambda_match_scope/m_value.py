import lib


def run():
    match "x":
        case lib.NAME:
            return lib.full()

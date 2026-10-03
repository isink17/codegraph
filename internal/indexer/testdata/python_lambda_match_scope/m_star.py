from lib import full


def run():
    match [0, 1]:
        case [_, *full]:
            return full()

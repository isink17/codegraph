from lib import full


def run():
    match (0, lambda: "or"):
        case [full] | (_, full):
            return full()

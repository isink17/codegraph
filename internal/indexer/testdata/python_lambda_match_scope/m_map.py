from lib import full


def run():
    match {"k": lambda: "map"}:
        case {"k": full}:
            return full()

from lib import full


def run():
    match {"k": lambda: "brace"}:
        case{"k": full}:
            return full()

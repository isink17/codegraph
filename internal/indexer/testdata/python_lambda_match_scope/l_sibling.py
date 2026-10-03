from lib import full


def other():
    return (lambda full: full())(lambda: "param")


def run():
    return full()

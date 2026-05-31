# Allows `python -m mesedi_curl` to invoke the same code path as the
# `mesedi-curl` console script defined in pyproject.toml. Both go
# through cli.main(), which returns the curl exit code.
import sys

from mesedi_curl.cli import main

if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

"""Create a Levis installer ZIP with explicit POSIX permissions on any host."""
import pathlib
import sys
import zipfile


def package(stage: pathlib.Path, destination: pathlib.Path) -> None:
    files = [('plugin', 0o755), ('frontend/index.html', 0o644)]
    for relative, _ in files:
        source = stage / relative
        if not source.is_file() or source.stat().st_size == 0:
            raise ValueError(f'missing or empty release file: {relative}')
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination.with_suffix(destination.suffix + '.tmp')
    try:
        with zipfile.ZipFile(temporary, 'w', compression=zipfile.ZIP_DEFLATED) as archive:
            for relative, mode in files:
                info = zipfile.ZipInfo(f'virtualis/{relative}')
                info.create_system = 3
                info.external_attr = (0o100000 | mode) << 16
                info.compress_type = zipfile.ZIP_DEFLATED
                # Stream the binary as well; packagers need not buffer it.
                with (stage / relative).open('rb') as source, archive.open(info, 'w') as output:
                    while chunk := source.read(64 << 10):
                        output.write(chunk)
        temporary.replace(destination)
    finally:
        temporary.unlink(missing_ok=True)


if __name__ == '__main__':
    if len(sys.argv) != 3:
        raise SystemExit('usage: package.py STAGE_DIRECTORY DESTINATION_ZIP')
    package(pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]))

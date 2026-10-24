"""Hermes tool and background event adapter for dots-duet-lite."""


def register(ctx):
    """Hermes directory-plugin entry point; the MCP worker runs separately."""
    from .plugin import register as register_plugin

    register_plugin(ctx)

import os
import signal
import sys
import tempfile
from typing import Annotated, Any

from mcp.server.fastmcp import FastMCP, Image
from pydantic import Field

import media_preview
import transcription
from mcp_config import resolve_host, resolve_port, resolve_transport
from parent_watchdog import install_stdio_parent_watchdog
from whatsapp import (
    MESSAGES_DB_PATH,
    msg_to_dict,
)
from whatsapp import (
    download_media as whatsapp_download_media,
)
from whatsapp import (
    get_chat as whatsapp_get_chat,
)
from whatsapp import (
    get_contact_chats as whatsapp_get_contact_chats,
)
from whatsapp import (
    get_direct_chat_by_contact as whatsapp_get_direct_chat_by_contact,
)
from whatsapp import (
    get_last_interaction as whatsapp_get_last_interaction,
)
from whatsapp import (
    get_message_context as whatsapp_get_message_context,
)
from whatsapp import (
    get_message_receipts as whatsapp_get_message_receipts,
)
from whatsapp import (
    get_sender_name as whatsapp_get_sender_name,
)
from whatsapp import (
    list_chats as whatsapp_list_chats,
)
from whatsapp import (
    list_messages as whatsapp_list_messages,
)
from whatsapp import (
    mark_messages_read as whatsapp_mark_messages_read,
)
from whatsapp import (
    search_contacts as whatsapp_search_contacts,
)
from whatsapp import (
    send_audio_message as whatsapp_audio_voice_message,
)
from whatsapp import (
    send_file as whatsapp_send_file,
)
from whatsapp import (
    send_message as whatsapp_send_message,
)
from whatsapp import (
    send_reaction as whatsapp_send_reaction,
)
from whatsapp import (
    bridge_call,
)
from whatsapp import (
    get_poll_results as whatsapp_get_poll_results,
)

# Initialize FastMCP server. Env-var handling is deferred to the __main__ block
# so importing this module never parses env vars or exits the process.
mcp = FastMCP("whatsapp")

PreviewDimension = Annotated[
    int,
    Field(
        strict=True,
        ge=media_preview.MIN_MAX_DIMENSION,
        le=media_preview.MAX_MAX_DIMENSION,
        description="Longest preview edge in pixels; must be an integer from 1 to 2048.",
    ),
]


@mcp.tool()
def search_contacts(query: str) -> list[dict[str, Any]]:
    """Search WhatsApp contacts by name or phone number.

    Args:
        query: Search term to match against contact names or phone numbers
    """
    contacts = whatsapp_search_contacts(query)
    return contacts


@mcp.tool()
def get_contact(
    identifier: str | None = None,
    phone_number: str | None = None,
    phone: str | None = None,
) -> dict[str, Any]:
    """Look up a WhatsApp contact by phone number, LID, or full JID.

    Automatically detects the identifier type and queries appropriately.

    Args:
        identifier: Phone number, LID, or full JID. Examples:
                    - "12025551234" (phone number)
                    - "35047067385985" (LID - numeric)
                    - "12025551234@s.whatsapp.net" (phone JID)
                    - "184125298348272@lid" (LID JID)
        phone_number: Backward-compatible alias for `identifier`.
        phone: Backward-compatible alias for `identifier` (matches README parameter name).

    Returns:
        Dictionary with jid, name, display_name, is_lid, and resolved status
    """
    if identifier is None:
        identifier = phone_number
    if identifier is None:
        identifier = phone
    if identifier is None:
        raise ValueError("Missing required argument: identifier (or phone_number / phone)")

    identifier = identifier.strip()
    if not identifier:
        raise ValueError("identifier must be non-empty")

    # Detect identifier type and normalize to JID.
    bare_numeric_digits: str | None = None
    if "@" in identifier:
        # Already a JID - use as-is
        jid = identifier
        is_lid = jid.endswith("@lid") or jid.split("@", 1)[-1] == "lid"
    else:
        digits = "".join(c for c in identifier if c.isdigit())
        if digits:
            # LIDs can overlap phone-number lengths, so bare numeric inputs try phone first.
            jid = f"{digits}@s.whatsapp.net"
            is_lid = False
            if identifier.isdigit():
                bare_numeric_digits = digits
        else:
            # Non-numeric and not a JID; try as-is.
            jid = identifier
            is_lid = False

    jid_user = jid.split("@", 1)[0]

    display_name: str | None = None
    resolved = False

    # Prefer chats table lookup via get_chat (works for both phone and LID contacts).
    candidates: list[tuple[str, bool]] = [(jid, is_lid)]
    if bare_numeric_digits:
        candidates.append((f"{bare_numeric_digits}@lid", True))

    chat = None
    for candidate_jid, candidate_is_lid in candidates:
        chat = whatsapp_get_chat(candidate_jid, include_last_message=False)
        if chat:
            jid = candidate_jid
            is_lid = candidate_is_lid
            jid_user = jid.split("@", 1)[0]
            break

    if chat and chat.get("name"):
        display_name = chat["name"]
        resolved = display_name not in (jid, jid_user)
    else:
        # Fallback: best-effort sender-name resolution (may use fuzzy LIKE lookup).
        display_name = whatsapp_get_sender_name(jid)
        resolved = display_name not in (jid, jid_user, identifier)

    return {
        "identifier": identifier,
        "jid": jid,
        "phone_number": jid_user if not is_lid else None,
        "lid": jid_user if is_lid else None,
        "name": display_name if resolved else jid_user,
        "display_name": display_name,
        "is_lid": is_lid,
        "resolved": resolved,
    }


@mcp.tool()
def list_messages(
    after: str | None = None,
    before: str | None = None,
    sender_phone_number: str | None = None,
    chat_jid: str | None = None,
    query: str | None = None,
    limit: int = 50,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1,
    sort_by: str = "newest",
) -> list[dict[str, Any]]:
    """Get WhatsApp messages matching specified criteria with optional context.

    Each message includes sender_display showing "Name (phone)" for easy identification.

    Args:
        after: ISO-8601 date string (e.g., "2026-01-01" or "2026-01-01T09:00:00")
        before: ISO-8601 date string (e.g., "2026-01-09" or "2026-01-09T18:00:00")
        sender_phone_number: Phone number to filter by sender (e.g., "12025551234")
        chat_jid: Chat JID to filter by (e.g., "12025551234@s.whatsapp.net" or group JID)
        query: Search term to filter messages by content
        limit: Max messages to return (default 50, max 500)
        page: Page number for pagination (default 0)
        include_context: Include surrounding messages for context (default True)
        context_before: Messages to include before each match (default 1)
        context_after: Messages to include after each match (default 1)
        sort_by: "newest" (default, most recent first) or "oldest" (chronological)
    """
    # Cap limit at 500 to prevent excessive queries
    limit = max(1, min(limit, 500))
    page = max(0, page)
    messages = whatsapp_list_messages(
        after=after,
        before=before,
        sender_phone_number=sender_phone_number,
        chat_jid=chat_jid,
        query=query,
        limit=limit,
        page=page,
        include_context=include_context,
        context_before=context_before,
        context_after=context_after,
        sort_by=sort_by,
    )
    return messages


@mcp.tool()
def list_chats(
    query: str | None = None,
    limit: int = 50,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active",
) -> list[dict[str, Any]]:
    """Get WhatsApp chats matching specified criteria.

    Args:
        query: Search term to filter chats by name or JID
        limit: Max chats to return (default 50, max 200)
        page: Page number for pagination (default 0)
        include_last_message: Include the last message in each chat (default True)
        sort_by: "last_active" (default, most recent first) or "name" (alphabetical)

    Returns:
        Chat dictionaries with jid, name, is_group, last_message_time, last_message,
        last_sender, last_is_from_me, last_read_time and unread. `last_read_time` is
        how far the chat has been read on any device (null if never reported); `unread`
        is true when the last message is inbound and newer than that marker, so chats
        already read on the phone are not reported as unread. Numeric fallback names
        are resolved from local contacts when possible, even while the bridge is offline.
    """
    # Cap limit at 200 to prevent excessive queries
    limit = max(1, min(limit, 200))
    page = max(0, page)
    chats = whatsapp_list_chats(
        query=query, limit=limit, page=page, include_last_message=include_last_message, sort_by=sort_by
    )
    return chats


@mcp.tool()
def get_chat(chat_jid: str, include_last_message: bool = True) -> dict[str, Any]:
    """Get WhatsApp chat metadata by JID.

    Args:
        chat_jid: The JID of the chat to retrieve
        include_last_message: Whether to include the last message (default True)

    Returns:
        Chat dictionary — same shape as list_chats, including last_read_time and unread.
    """
    chat = whatsapp_get_chat(chat_jid, include_last_message)
    return chat


@mcp.tool()
def get_direct_chat_by_contact(sender_phone_number: str) -> dict[str, Any]:
    """Get WhatsApp chat metadata by sender phone number.

    Args:
        sender_phone_number: The phone number to search for
    """
    chat = whatsapp_get_direct_chat_by_contact(sender_phone_number)
    return chat


@mcp.tool()
def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> list[dict[str, Any]]:
    """Get all WhatsApp chats involving the contact.

    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    limit = max(1, min(limit, 200))
    page = max(0, page)
    chats = whatsapp_get_contact_chats(jid, limit, page)
    return chats


@mcp.tool()
def get_last_interaction(jid: str) -> dict[str, Any]:
    """Get most recent WhatsApp message involving the contact.

    Args:
        jid: The JID of the contact to search for

    Returns:
        Message dictionary with id, timestamp, sender, content, etc. or empty dict if not found.
    """
    message = whatsapp_get_last_interaction(jid)
    return message if message else {}


@mcp.tool()
def get_message_context(message_id: str, before: int = 5, after: int = 5) -> dict[str, Any]:
    """Get context around a specific WhatsApp message.

    Args:
        message_id: The ID of the message to get context for
        before: Number of messages to include before the target message (default 5)
        after: Number of messages to include after the target message (default 5)
    """
    context = whatsapp_get_message_context(message_id, before, after)
    return {
        "message": msg_to_dict(context.message),
        "before": [msg_to_dict(message) for message in context.before],
        "after": [msg_to_dict(message) for message in context.after],
    }


@mcp.tool()
def get_message_receipts(message_id: str, chat_jid: str) -> dict[str, Any]:
    """Who received and who read one of your sent WhatsApp messages (blue ticks).

    Args:
        message_id: The ID of a message you sent
        chat_jid: The JID of its chat (1:1 or group)

    Returns:
        {"receipts": [...]}: per reader their furthest status ("delivered", "read",
        or "played" for voice notes) and when it came. Recorded only from the
        bridge version that tracks receipts onward; people who disabled read
        receipts only ever show as delivered.
    """
    return {"receipts": whatsapp_get_message_receipts(message_id, chat_jid)}


@mcp.tool()
def send_message(
    recipient: str,
    message: str,
    quoted_message_id: str = "",
    quoted_sender_jid: str = "",
    quoted_content: str = "",
    mentions: list[str] | None = None,
    link_preview: dict[str, str] | None = None,
) -> dict[str, Any]:
    """Send a WhatsApp message to a person or group. For group chats use the JID.

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        message: The message text to send
        quoted_message_id: ID of the message to reply to (optional). When set, the sent
                           message will appear as a quoted reply in WhatsApp.
        quoted_sender_jid: Full JID of the author of the quoted message. Required for
                           group replies so WhatsApp renders the correct attribution.
        quoted_content: Text content of the quoted message, used for the reply preview.
                        Only plain text is supported; media previews are not included.
        mentions: Users to @-mention, as phone numbers with country code but no + (e.g.
                  ["420601234567"]) or JIDs. For each entry the message text must contain
                  a matching "@<number>" token (e.g. "hi @420601234567"), otherwise the
                  mention won't render on recipients' devices. Only meaningful in groups.
        link_preview: The card shown under a link in the message (optional), as
                  {"url", "title", "description", "thumbnail"}: WhatsApp shows a sent
                  link's preview only when the sender attaches it. `url` must appear in
                  the message; `thumbnail` is an optional base64 JPEG (up to 100 KB).

    Returns:
        A dictionary containing success status and a status message
    """
    # Validate input
    if not recipient:
        return {"success": False, "message": "Recipient must be provided"}

    # Call the whatsapp_send_message function with the unified recipient parameter
    success, status_message = whatsapp_send_message(
        recipient, message, quoted_message_id, quoted_sender_jid, quoted_content, mentions, link_preview
    )
    return {"success": success, "message": status_message}


@mcp.tool()
def send_reaction(
    recipient: str,
    message_id: str,
    emoji: str,
    from_me: bool = False,
    sender_jid: str = "",
) -> dict[str, Any]:
    """Send (or remove) a reaction to a WhatsApp message.

    Args:
        recipient: The chat JID the message belongs to (e.g., "12025551234@s.whatsapp.net"
                   or a group JID like "123456789@g.us")
        message_id: The ID of the message to react to
        emoji: The reaction emoji (e.g., "👍"). Pass an empty string to remove the reaction.
        from_me: Whether the original message was sent by the current user (default False)
        sender_jid: JID of the original message sender — required for group messages when
                    from_me is False so the bridge can build the correct WhatsApp key

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_send_reaction(recipient, message_id, emoji, from_me, sender_jid)
    return {"success": success, "message": status_message}


@mcp.tool()
def edit_message(chat_jid: str, message_id: str, text: str) -> dict[str, Any]:
    """Change the text of a text message you sent, within 15 minutes of sending
    (WhatsApp's limit). Everyone in the chat sees it marked as edited.

    Args:
        chat_jid: The chat the message is in (from list_messages)
        message_id: The id of your message
        text: The new text
    """
    return bridge_call("POST", "/edit", {"chat_jid": chat_jid, "message_id": message_id, "text": text})


@mcp.tool()
def delete_message(chat_jid: str, message_id: str) -> dict[str, Any]:
    """Delete a message you sent for everyone, within 48 hours of sending.
    It cannot be undone; the chat shows "This message was deleted".

    Args:
        chat_jid: The chat the message is in (from list_messages)
        message_id: The id of your message
    """
    return bridge_call("POST", "/revoke", {"chat_jid": chat_jid, "message_id": message_id})


@mcp.tool()
def request_history(chat_jid: str, count: int = 50) -> dict[str, Any]:
    """Ask the phone for messages of a chat older than the oldest one stored here.
    They arrive in the background over the next minute (the phone decides how many);
    read them with list_messages afterwards.

    Args:
        chat_jid: The chat to fetch older messages for
        count: How many older messages to ask for, up to 500 (default 50)
    """
    return bridge_call("POST", "/history", {"chat_jid": chat_jid, "count": count})


@mcp.tool()
def create_group(name: str, participants: list[str]) -> dict[str, Any]:
    """Create a new WhatsApp group with this account as admin. Participants are
    added at once and see the group immediately: confirm name and members first.

    Args:
        name: Group name, 1-25 characters
        participants: Phone numbers with country code (e.g. "393331234567" or
                      "+39 333 123 4567") or user JIDs, without your own number

    Returns:
        ok, the group "jid" (use it with send_message), "done" (people added), and
        "failed" (people WhatsApp would not add, usually for their privacy settings)
        with an "invite_link" to send them instead
    """
    return bridge_call("POST", "/group/create", {"name": name, "participants": participants})


@mcp.tool()
def get_group_info(group_jid: str) -> dict[str, Any]:
    """A group's name, description, settings and members (phone, admin, owner).

    Args:
        group_jid: The group's JID (…@g.us)
    """
    return bridge_call("GET", "/group/info", params={"jid": group_jid})


@mcp.tool()
def update_group_participants(group_jid: str, action: str, participants: list[str]) -> dict[str, Any]:
    """Add or remove people in a group you administer, or make them admins (promote)
    or not (demote). People see the change at once: confirm first.

    Args:
        group_jid: The group's JID (…@g.us)
        action: "add", "remove", "promote" or "demote"
        participants: Phone numbers with country code or user JIDs

    Returns:
        "done", and "failed" with WhatsApp's error code per person; when adding,
        an "invite_link" for those WhatsApp would not add
    """
    return bridge_call("POST", "/group/participants", {"group": group_jid, "action": action, "participants": participants})


@mcp.tool()
def update_group_settings(
    group_jid: str,
    name: str | None = None,
    description: str | None = None,
    only_admins_send: bool | None = None,
    only_admins_edit_info: bool | None = None,
) -> dict[str, Any]:
    """Change a group's name, description, or who may send messages and edit its
    info. Pass only what should change; members see each change in the chat.

    Args:
        group_jid: The group's JID (…@g.us)
        name: New name, 1-25 characters
        description: New description ("" clears it)
        only_admins_send: True so only admins can send messages
        only_admins_edit_info: True so only admins can change name, description and photo
    """
    payload: dict[str, Any] = {"group": group_jid}
    for key, value in (("name", name), ("description", description),
                       ("only_admins_send", only_admins_send), ("only_admins_edit_info", only_admins_edit_info)):
        if value is not None:
            payload[key] = value
    return bridge_call("POST", "/group/settings", payload)


@mcp.tool()
def set_group_photo(group_jid: str, image_path: str = "", remove: bool = False) -> dict[str, Any]:
    """Change a group's photo, or remove it. Members see the change in the chat.

    The image (JPEG, PNG or GIF) is cropped to a centered square and scaled to
    640x640, so put the subject in the middle. It must sit in a folder the
    bridge may read from, as for send_file.

    Args:
        group_jid: The group's JID (…@g.us)
        image_path: Absolute path to the new photo
        remove: True to remove the current photo instead
    """
    payload: dict[str, Any] = {"group": group_jid}
    if remove:
        payload["remove"] = True
    else:
        payload["image_path"] = image_path
    return bridge_call("POST", "/group/photo", payload)


@mcp.tool()
def get_group_invite_link(group_jid: str, reset: bool = False) -> dict[str, Any]:
    """The invite link of a group you administer.

    Args:
        group_jid: The group's JID (…@g.us)
        reset: True to revoke the current link and make a new one (the old one stops working)
    """
    return bridge_call("GET", "/group/invite", params={"jid": group_jid, "reset": "1" if reset else "0"})


@mcp.tool()
def leave_group(group_jid: str) -> dict[str, Any]:
    """Leave a group. Members see that you left; you need a new invite to come back.

    Args:
        group_jid: The group's JID (…@g.us)
    """
    return bridge_call("POST", "/group/leave", {"group": group_jid})


@mcp.tool()
def join_group(invite_link: str) -> dict[str, Any]:
    """Join a group from its invite link (https://chat.whatsapp.com/…). Members see you join.

    Args:
        invite_link: The invite link, or just its code
    """
    return bridge_call("POST", "/group/join", {"link": invite_link})


@mcp.tool()
def send_poll(recipient: str, question: str, options: list[str], multiple_answers: bool = False) -> dict[str, Any]:
    """Send a poll to a person or group. Read the votes later with get_poll_results.

    Args:
        recipient: Phone number with country code, or a user or group JID
        question: The poll's question
        options: 2 to 12 distinct answers
        multiple_answers: True to let people pick more than one answer

    Returns:
        ok, "message_id" and "chat_jid" (pass both to get_poll_results)
    """
    return bridge_call("POST", "/poll", {"recipient": recipient, "question": question,
                                          "options": options, "multiple": multiple_answers})


@mcp.tool()
def get_poll_results(chat_jid: str, message_id: str) -> dict[str, Any]:
    """Votes on a poll in a chat: each option with who chose it. Works for polls
    sent or received while the bridge was running (or seen in history sync); votes
    cast before the bridge saw the poll are missing.

    Args:
        chat_jid: The chat the poll is in
        message_id: The poll message's id (from send_poll or list_messages)
    """
    return whatsapp_get_poll_results(message_id, chat_jid)


@mcp.tool()
def send_location(recipient: str, latitude: float, longitude: float, name: str = "", address: str = "") -> dict[str, Any]:
    """Send a map pin.

    Args:
        recipient: Phone number with country code, or a user or group JID
        latitude: Latitude in degrees (e.g. 41.9028)
        longitude: Longitude in degrees (e.g. 12.4964)
        name: Optional place name shown on the pin
        address: Optional address shown under the name
    """
    return bridge_call("POST", "/send-location", {"recipient": recipient, "latitude": latitude,
                                                   "longitude": longitude, "name": name, "address": address})


@mcp.tool()
def send_contact(recipient: str, name: str, phone: str) -> dict[str, Any]:
    """Send a contact card someone can tap to save or message.

    Args:
        recipient: Phone number with country code, or a user or group JID
        name: The contact's name as it should appear on the card
        phone: The contact's phone number with country code
    """
    return bridge_call("POST", "/send-contact", {"recipient": recipient, "name": name, "phone": phone})


@mcp.tool()
def check_numbers(phones: list[str]) -> dict[str, Any]:
    """Whether phone numbers are on WhatsApp, with their "about" text and verified
    business name when they have one. Nothing is sent to them.

    Args:
        phones: 1-50 phone numbers with country code
    """
    return bridge_call("POST", "/check-numbers", {"phones": phones})


@mcp.tool()
def set_disappearing_timer(chat: str, timer: str) -> dict[str, Any]:
    """Turn disappearing messages on or off in a chat or group. Everyone in the
    chat sees the change; new messages then vanish after the timer.

    Args:
        chat: Phone number with country code, or a user or group JID
        timer: "off", "24h", "7d" or "90d"
    """
    return bridge_call("POST", "/disappearing", {"chat": chat, "timer": timer})


@mcp.tool()
def block_contact(contact: str, block: bool = True) -> dict[str, Any]:
    """Block someone (they can no longer message or call you), or unblock them.

    Args:
        contact: Phone number with country code, or a user JID
        block: True to block, False to unblock
    """
    return bridge_call("POST", "/block", {"contact": contact, "action": "block" if block else "unblock"})


@mcp.tool()
def mark_messages_read(
    message_ids: list[str],
    chat_jid: str,
    sender_jid: str = "",
    timestamp: str | None = None,
) -> dict[str, Any]:
    """Mark selected WhatsApp messages as read and send read receipts.

    This is an explicit external side effect. All message IDs must belong to the
    same chat and sender.

    Args:
        message_ids: IDs of the messages to mark as read
        chat_jid: JID of the chat containing the messages
        sender_jid: JID or bare phone number of the original sender; required for groups
        timestamp: Optional RFC 3339 read timestamp; defaults to the current time

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_mark_messages_read(message_ids, chat_jid, sender_jid, timestamp)
    return {"success": success, "message": status_message}


@mcp.tool()
def send_file(recipient: str, media_path: str, caption: str = "", as_document: bool = False) -> dict[str, Any]:
    """Send a file (image, video, document) via WhatsApp, optionally with a caption.

    When `caption` is provided, the file and text arrive as a single
    attachment-with-caption message (one bubble in the WA UI), instead of
    needing a separate follow-up send_message call. For group chats use the JID.

    Args:
        recipient: Either a phone number with country code (no + or symbols),
                 or a JID (e.g., "123456789@s.whatsapp.net" or "123456789@g.us")
        media_path: Absolute path to the media file (image, video, document)
        caption: Optional text rendered with the file as a caption. Omit for a
                 bare attachment.
        as_document: Send a photo or video as a file instead (full quality, no
                 WhatsApp compression; the recipient gets it as a document).

    Returns:
        A dictionary containing success status and a status message
    """

    # Call the whatsapp_send_file function
    success, status_message = whatsapp_send_file(recipient, media_path, caption, as_document)
    return {"success": success, "message": status_message}


@mcp.tool()
def send_audio_message(recipient: str, media_path: str) -> dict[str, Any]:
    """Send any audio file as a WhatsApp audio message to the specified recipient. For group messages use the JID. If it errors due to ffmpeg not being installed, use send_file instead.

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the audio file to send (will be converted to Opus .ogg if it's not a .ogg file)

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_audio_voice_message(recipient, media_path)
    return {"success": success, "message": status_message}


@mcp.tool()
def download_media(message_id: str, chat_jid: str) -> dict[str, Any]:
    """Download media from a WhatsApp message and get the local file path.

    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message

    Returns:
        A dictionary containing success status, a status message, and the file path if successful
    """
    file_path = whatsapp_download_media(message_id, chat_jid)

    if file_path:
        return {"success": True, "message": "Media downloaded successfully", "file_path": file_path}
    else:
        return {"success": False, "message": "Failed to download media"}


@mcp.tool()
def view_media(
    message_id: str,
    chat_jid: str,
    max_dimension: PreviewDimension = media_preview.DEFAULT_MAX_DIMENSION,
) -> Any:
    """View the media of a WhatsApp message as an image.

    download_media only returns a local file path, which a client without
    filesystem access cannot open. This returns the picture itself instead.
    Videos return their first frame, which is enough to tell what was sent.
    Both are downscaled so a single photo cannot flood the context. Voice notes
    are not images — use transcribe_audio or read the transcript from the
    message content with list_messages.

    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message
        max_dimension: Longest edge of the returned image in pixels (default 1024)

    Returns:
        Image content on success, otherwise a dictionary explaining why not
    """
    try:
        media_preview.validate_max_dimension(max_dimension)
    except media_preview.PreviewError as exc:
        return {"success": False, "message": str(exc)}

    file_path = whatsapp_download_media(message_id, chat_jid)
    if not file_path:
        return {"success": False, "message": "Failed to download media"}

    with tempfile.TemporaryDirectory() as work_dir:
        try:
            data, image_format = media_preview.render_preview(file_path, max_dimension=max_dimension, work_dir=work_dir)
        except media_preview.PreviewError as exc:
            return {"success": False, "message": str(exc), "file_path": file_path}

    return Image(data=data, format=image_format)


@mcp.tool()
def transcribe_audio(message_id: str, chat_jid: str, force: bool = False) -> dict[str, Any]:
    """Transcribe a WhatsApp voice note and return its text.

    Runs whisper.cpp locally by default, or sends audio to the operator's
    configured OpenAI-compatible endpoint. The transcript is also written into
    the message's empty content field, so afterwards it is readable through list_messages by any
    client — including one with no filesystem access — without transcribing
    again.

    Call this for a voice note whose content field is still empty. Requires
    whisper.cpp, FFmpeg, and WHISPER_MODEL for the default provider; alternatively
    configure WHATSAPP_TRANSCRIPTION_PROVIDER=openai_compatible, URL and MODEL.

    Args:
        message_id: The ID of the message containing the voice note
        chat_jid: The JID of the chat containing the message
        force: Transcribe again even when a transcript is already stored

    Returns:
        A dictionary with success status and the transcript
    """
    if not force:
        existing = transcription.stored_transcript(MESSAGES_DB_PATH, message_id, chat_jid)
        if existing:
            return {"success": True, "message": "Transcript already stored", "transcript": existing}

    # WhatsApp expires media server-side after a few weeks, so the file has to
    # be on disk. This is a no-op when the bridge already downloaded it.
    file_path = whatsapp_download_media(message_id, chat_jid)
    if not file_path:
        return {
            "success": False,
            "message": (
                "Could not obtain the audio file. WhatsApp expires media after a while, "
                "so an old voice note may no longer be downloadable."
            ),
        }
    if not transcription.is_audio(file_path):
        return {
            "success": False,
            "message": "This message is not audio. Use download_media instead.",
            "file_path": file_path,
        }

    try:
        provider = transcription.provider_name()
        model = transcription.configured_model(provider)
        with tempfile.TemporaryDirectory() as work_dir:
            text = transcription.transcribe_file(file_path, work_dir, model=model, provider=provider)
    except transcription.TranscriptionError as exc:
        return {"success": False, "message": str(exc)}

    if not transcription.store_transcript(MESSAGES_DB_PATH, message_id, chat_jid, text, model, provider=provider):
        # The words are worth returning even when the row could not be updated,
        # but say so: without the row, list_messages will not show them.
        return {
            "success": True,
            "message": "Transcribed, but the transcript could not be stored on the message row",
            "transcript": text,
        }

    return {
        "success": True,
        "message": "Transcribed",
        "transcript": f"{transcription.label(model, provider)}{text}",
    }


def shutdown_handler(signum, frame):
    """Handle shutdown signals gracefully to prevent zombie processes."""
    sys.exit(0)


if __name__ == "__main__":
    # Capture before any await — os.getppid() is dynamic.
    parent_pid = os.getppid()
    # Register signal handlers for clean shutdown
    signal.signal(signal.SIGINT, shutdown_handler)
    signal.signal(signal.SIGTERM, shutdown_handler)

    # Resolve the transport first: host/port are only used (and validated) for the
    # network transports, so a bad WHATSAPP_MCP_PORT can't break a stdio launch.
    # The localhost default keeps a remote server unreachable until explicitly opened up.
    try:
        transport = resolve_transport(os.getenv("WHATSAPP_MCP_TRANSPORT"))
        if transport != "stdio":
            mcp.settings.host = resolve_host(os.getenv("WHATSAPP_MCP_HOST"))
            mcp.settings.port = resolve_port(os.getenv("WHATSAPP_MCP_PORT"))
            # stdout is reserved for the protocol on stdio; log startup to stderr.
            print(
                f"WhatsApp MCP server listening on {mcp.settings.host}:{mcp.settings.port} via {transport}",
                file=sys.stderr,
            )
    except ValueError as exc:
        raise SystemExit(str(exc)) from None

    if transport == "stdio":
        install_stdio_parent_watchdog("WHATSAPP_PARENT_WATCHDOG_S", parent_pid=parent_pid)
    mcp.run(transport=transport)

-- Mouse support for the OSD menu, ported from upstream's mouse.lua.
--
-- mpv gives us MOUSE_MOVE/MOUSE_BTN0 bindings but the OSD menu is drawn by the
-- shim (multi-line `show-text` over a background box), so we translate the
-- pointer position into a menu row and report it to the shim:
--
--   shim-menu-select <index>  (0 = the "<-- back" title, 1..n = rows)
--   shim-menu-click           (activate the row under the pointer)
--
-- Enabled by the shim through the `menu_mouse` setting; the script is only
-- loaded when that is on.

local last_idx = nil

local function menu_handler()
    local x, y = mp.get_mouse_pos()
    if x == nil or y == nil then
        return
    end
    local oh = mp.get_property_native("osd-height")
    local ow = mp.get_property_native("osd-width")
    if oh == nil or ow == nil or oh == 0 then
        return
    end
    -- The shim draws the title on the first line and the entries below it, in
    -- osd-font-size 40 over a background box; walk the same geometry.
    local line_h = 40 / mp.get_property_native("osd-height") -- fraction of a line
    local idx = math.floor((y * 1000 / oh - 40) / 55)
    if idx ~= last_idx then
        last_idx = idx
        mp.commandv("script-message", "shim-menu-select", tostring(idx))
    end
end

local function menu_click_handler()
    last_idx = nil
    menu_handler()
    mp.commandv("script-message", "shim-menu-click")
end

local function enable(enabled)
    if enabled then
        mp.add_key_binding("MOUSE_MOVE", "shim_mouse_move_handler", menu_handler)
        mp.add_key_binding("MOUSE_BTN0", "shim_mouse_click_handler", menu_click_handler)
    else
        mp.remove_key_binding("shim_mouse_move_handler")
        mp.remove_key_binding("shim_mouse_click_handler")
    end
end

mp.add_key_binding("MOUSE_MOVE", "shim_mouse_move_handler", menu_handler)
mp.register_event("client-message", function(event)
    local args = event["args"] or {}
    if args[1] == "shim-menu-enable" then
        enable(args[2] == "True")
    end
end)

import test from "node:test";
import assert from "node:assert/strict";
import { parseChatCommand, chatControlsBase, chatControlsURL } from "./chat-commands.ts";

test("chat commands preserve payload and leave btw/plain text to existing routing", () => {
 assert.equal(parseChatCommand("/btw question"),null);
 assert.equal(parseChatCommand("/queue /retry").text,"/retry");
 assert.deepEqual(parseChatCommand("/steer focus auth"),{kind:"steer",text:"focus auth"});
 assert.equal(parseChatCommand("/compress here 0").kind,"invalid");
 assert.equal(parseChatCommand("plain text"),null);
 assert.deepEqual(parseChatCommand("/compress focus login 3"),{kind:"compact",options:{focus:"focus login",keep_recent:3}});
 assert.deepEqual(parseChatCommand("/compress"),{kind:"compact",options:{focus:"",keep_recent:2}});
 assert.equal(parseChatCommand("/queue").kind,"invalid");
 assert.equal(parseChatCommand("/retry unexpected").kind,"invalid");
 assert.deepEqual(parseChatCommand("/branch Investigation"),{kind:"branch",title:"Investigation"});
 assert.equal(parseChatCommand("/unknown x"),null);
});
test("control URLs preserve the segment query after action paths",()=>{
 const ref={conversation_id:"0",task_id:"12",exploration_id:"14",main_seg:2};
 assert.equal(chatControlsBase(ref),"/tasks/12/chat/controls?seg=2");
 assert.equal(chatControlsURL(ref,"inputs/x"),"/tasks/12/chat/controls/inputs/x?seg=2");
 assert.equal(chatControlsURL(ref,"inputs/x","expected_revision=3"),"/tasks/12/chat/controls/inputs/x?seg=2&expected_revision=3");
 assert.equal(chatControlsBase({conversation_id:"8",task_id:"0",exploration_id:"0",main_seg:0}),"/conversations/8/controls");
});

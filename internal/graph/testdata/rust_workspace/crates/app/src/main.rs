use core_lib::Widget;
use core_lib::util::help;

fn main() {
    let w = Widget::new("app");
    let _ = w.name;
    let _ = core_lib::build();
    let _ = help();
}
